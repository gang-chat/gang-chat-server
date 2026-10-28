package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/releases"
	"github.com/zhuangkaiyi/gang-chat/server/internal/storage"
)

// maxUploadBytes matches the reverse proxy's request body limit.
const maxUploadBytes = 2 << 30

var errObjectNotFound = errors.New("object not found")

type objectSummary struct {
	Key          string `json:"key"`
	Size         int64  `json:"size"`
	LastModified string `json:"lastModified,omitempty"`
}

type objectList struct {
	Bucket                string          `json:"bucket"`
	Prefix                string          `json:"prefix"`
	Prefixes              []string        `json:"prefixes"`
	Objects               []objectSummary `json:"objects"`
	IsTruncated           bool            `json:"isTruncated"`
	NextContinuationToken string          `json:"nextContinuationToken,omitempty"`
}

type objectMetadata struct {
	objectSummary
	Bucket      string            `json:"bucket"`
	ContentType string            `json:"contentType,omitempty"`
	Metadata    map[string]string `json:"metadata"`
}

// objectStore is the server's storage directory, browsed like a bucket: keys
// are paths below it and "/" separates folders. Release sync and the file
// browser go through it, so tests can swap in memory.
type objectStore interface {
	List(ctx context.Context, prefix, delimiter, token string, maxKeys int32) (objectList, error)
	Head(ctx context.Context, key string) (objectMetadata, error)
	Get(ctx context.Context, key string) (io.ReadCloser, string, error)
	Put(ctx context.Context, key string, body io.Reader) error
	Delete(ctx context.Context, key string) error
}

type fileStore struct {
	files *storage.Files
}

// List pages through the files under prefix in key order. With a delimiter,
// keys that continue past it are folded into one folder entry; the token is
// the last entry of the previous page.
func (s fileStore) List(_ context.Context, prefix, delimiter, token string, maxKeys int32) (objectList, error) {
	files, err := s.files.List(prefix)
	if errors.Is(err, storage.ErrInvalidKey) {
		return objectList{}, fail(http.StatusBadRequest, "VALIDATION_ERROR", "Invalid prefix")
	}
	if err != nil {
		return objectList{}, err
	}
	list := objectList{Bucket: s.files.Root(), Prefix: prefix, Prefixes: []string{}, Objects: []objectSummary{}}
	last := ""
	count := int32(0)
	for _, file := range files {
		entry, folder := file.Key, false
		if delimiter != "" {
			if i := strings.Index(file.Key[len(prefix):], delimiter); i >= 0 {
				entry, folder = file.Key[:len(prefix)+i+len(delimiter)], true
			}
		}
		if (token != "" && entry <= token) || entry == last {
			continue
		}
		if count == maxKeys {
			list.IsTruncated = true
			list.NextContinuationToken = last
			break
		}
		last = entry
		count++
		if folder {
			list.Prefixes = append(list.Prefixes, entry)
		} else {
			list.Objects = append(list.Objects, summarize(file))
		}
	}
	return list, nil
}

func summarize(file storage.File) objectSummary {
	return objectSummary{Key: file.Key, Size: file.Size, LastModified: file.ModTime.UTC().Format(time.RFC3339)}
}

func (s fileStore) Head(_ context.Context, key string) (objectMetadata, error) {
	file, err := s.files.Stat(key)
	if errors.Is(err, fs.ErrNotExist) {
		return objectMetadata{}, errObjectNotFound
	}
	if err != nil {
		return objectMetadata{}, err
	}
	return objectMetadata{objectSummary: summarize(file), Bucket: s.files.Root(), ContentType: contentTypeOf(key), Metadata: map[string]string{}}, nil
}

func (s fileStore) Get(_ context.Context, key string) (io.ReadCloser, string, error) {
	file, _, err := s.files.Open(key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", errObjectNotFound
	}
	if err != nil {
		return nil, "", err
	}
	return file, contentTypeOf(key), nil
}

func (s fileStore) Put(_ context.Context, key string, body io.Reader) error {
	return s.files.Put(key, body)
}

func (s fileStore) Delete(_ context.Context, key string) error {
	return s.files.Delete(key)
}

// contentTypeOf guesses a stored file's type from its name; nothing records
// the type a file was uploaded with.
func contentTypeOf(key string) string {
	if strings.HasPrefix(key, storage.ReleasePrefix) {
		return releases.FileContentType(path.Base(key))
	}
	if contentType := mime.TypeByExtension(path.Ext(key)); contentType != "" {
		return contentType
	}
	return "application/octet-stream"
}

func hasControlCharacter(value string) bool {
	return strings.IndexFunc(value, func(r rune) bool { return r < 32 || r == 127 }) >= 0
}

// validObjectKey refuses keys that would read as paths outside the storage
// directory, or as hidden files.
func validObjectKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 2048 || hasControlCharacter(key) || strings.ContainsRune(key, '\\') {
		return "", fail(http.StatusBadRequest, "INVALID_S3_OBJECT_KEY", "Invalid file path")
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || strings.HasPrefix(part, ".") {
			return "", fail(http.StatusBadRequest, "INVALID_S3_OBJECT_KEY", "Invalid file path")
		}
	}
	return key, nil
}

func (h *Handler) requireObjects() (objectStore, error) {
	if h.Objects == nil {
		return nil, fail(http.StatusConflict, "S3_NOT_CONFIGURED", "Set storage_path in the server's config.json to browse files")
	}
	return h.Objects, nil
}

func (h *Handler) listObjects(c *gin.Context) (any, error) {
	store, err := h.requireObjects()
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSpace(c.Query("prefix"))
	if len(prefix) > 1024 {
		return nil, fail(http.StatusBadRequest, "VALIDATION_ERROR", "Prefix is too long")
	}
	maxKeys := clampQueryInt(c, "maxKeys", 500, 1, 1000)
	return store.List(c.Request.Context(), prefix, "/", strings.TrimSpace(c.Query("continuationToken")), int32(maxKeys))
}

func (h *Handler) headObject(c *gin.Context) (any, error) {
	store, err := h.requireObjects()
	if err != nil {
		return nil, err
	}
	key, err := validObjectKey(c.Query("key"))
	if err != nil {
		return nil, err
	}
	meta, err := store.Head(c.Request.Context(), key)
	if errors.Is(err, errObjectNotFound) {
		return nil, fail(http.StatusNotFound, "S3_OBJECT_NOT_FOUND", "File not found")
	}
	return meta, err
}

func (h *Handler) downloadObject(c *gin.Context) (any, error) {
	store, err := h.requireObjects()
	if err != nil {
		return nil, err
	}
	key, err := validObjectKey(c.Query("key"))
	if err != nil {
		return nil, err
	}
	body, contentType, err := store.Get(c.Request.Context(), key)
	if errors.Is(err, errObjectNotFound) {
		err = fail(http.StatusNotFound, "S3_OBJECT_NOT_FOUND", "File not found")
	}
	h.audit(c, "s3.download", key, "", err)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	name := key[strings.LastIndex(key, "/")+1:]
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	c.DataFromReader(http.StatusOK, -1, contentType, body, nil)
	return nil, nil
}

type uploadResult struct {
	Bucket      string `json:"bucket"`
	Key         string `json:"key"`
	Overwritten bool   `json:"overwritten"`
	Filename    string `json:"filename"`
}

func (h *Handler) uploadObject(c *gin.Context) (any, error) {
	store, err := h.requireObjects()
	if err != nil {
		return nil, err
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadBytes)
	file, err := c.FormFile("file")
	if err != nil {
		return nil, fail(http.StatusBadRequest, "MISSING_FILE", "Upload file is required")
	}
	rawKey := c.PostForm("key")
	if strings.TrimSpace(rawKey) == "" {
		rawKey = file.Filename
	}
	key, err := validObjectKey(rawKey)
	if err != nil {
		return nil, err
	}
	result := uploadResult{Bucket: h.Bucket, Key: key, Filename: file.Filename}
	err = func() error {
		overwrite := formBool(c.PostForm("overwrite"))
		if !overwrite {
			_, err := store.Head(c.Request.Context(), key)
			if err == nil {
				return fail(http.StatusConflict, "S3_OBJECT_ALREADY_EXISTS", "File already exists")
			}
			if !errors.Is(err, errObjectNotFound) {
				return err
			}
		} else if strings.TrimSpace(c.PostForm("overwriteConfirmation")) != key {
			return fail(http.StatusBadRequest, "DESTRUCTIVE_CONFIRMATION_REQUIRED", "Type the exact file path to overwrite it")
		}
		body, err := file.Open()
		if err != nil {
			return err
		}
		defer body.Close()
		if err := store.Put(c.Request.Context(), key, body); err != nil {
			return err
		}
		result.Overwritten = overwrite
		return nil
	}()
	h.audit(c, "s3.upload", key, fmt.Sprintf("%d bytes", file.Size), err)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func formBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes":
		return true
	}
	return false
}

func (h *Handler) deleteObject(c *gin.Context) (any, error) {
	store, err := h.requireObjects()
	if err != nil {
		return nil, err
	}
	var req struct {
		Key          string `json:"key"`
		Confirmation string `json:"confirmation"`
	}
	if err := bindJSON(c, &req); err != nil {
		return nil, err
	}
	key, err := validObjectKey(req.Key)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Confirmation) != key {
		err = fail(http.StatusBadRequest, "DESTRUCTIVE_CONFIRMATION_REQUIRED", "Type the exact file path to delete it")
	} else {
		err = store.Delete(c.Request.Context(), key)
	}
	h.audit(c, "s3.delete", key, "", err)
	if err != nil {
		return nil, err
	}
	return gin.H{"deleted": true}, nil
}
