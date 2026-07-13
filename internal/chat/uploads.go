package chat

import (
	"database/sql"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
)

const multipartFormOverheadBytes int64 = 1024 * 1024

var errUploadTooLarge = errors.New("uploaded file exceeds size limit")

func (h *Handler) uploadFile(c *gin.Context) {
	isImageUpload := strings.Contains(c.FullPath(), "/uploads/images")
	maxFileBytes := h.uploadLimitBytes(isImageUpload)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxFileBytes+multipartFormOverheadBytes)

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		if uploadTooLarge(err) {
			h.jsonError(c, http.StatusRequestEntityTooLarge, "payload_too_large", "uploaded file is too large")
			return
		}
		h.jsonError(c, http.StatusBadRequest, "bad_request", "file is required")
		return
	}
	defer file.Close()
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}
	if header.Size > maxFileBytes {
		h.jsonError(c, http.StatusRequestEntityTooLarge, "payload_too_large", "uploaded file is too large")
		return
	}
	purpose := c.PostForm("purpose")
	if purpose == "" {
		if isImageUpload {
			purpose = "image"
		} else {
			purpose = "message_file"
		}
	}
	mimeType := canonicalMimeType(header.Header.Get("Content-Type"))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	assetStore := h.assetStore()
	id := newID("ast")
	filenameMime := mimeType
	if isImageUpload && filenameMime == "application/octet-stream" {
		filenameMime = "image/png"
	}
	filename := safeAssetFilename(header.Filename, filenameMime)
	tmp, err := os.CreateTemp("", "gang-chat-asset-*"+filepath.Ext(filename))
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "create upload temp file failed")
		return
	}
	assetPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(assetPath)
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "create upload temp file failed")
		return
	}
	defer func() { _ = os.Remove(assetPath) }()

	sizeBytes, detectedMime, err := writeUploadedAsset(assetPath, file, maxFileBytes)
	if err != nil {
		_ = os.Remove(assetPath)
		if uploadTooLarge(err) {
			h.jsonError(c, http.StatusRequestEntityTooLarge, "payload_too_large", "uploaded file is too large")
			return
		}
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "store uploaded file failed")
		return
	}
	detectedMime = canonicalMimeType(detectedMime)
	if detectedMime != "" && detectedMime != "application/octet-stream" {
		mimeType = detectedMime
	}
	if isImageUpload && !strings.HasPrefix(mimeType, "image/") {
		_ = os.Remove(assetPath)
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "image file is required")
		return
	}
	var widthValue any
	var heightValue any
	thumb := any(nil)
	if strings.HasPrefix(mimeType, "image/") {
		thumb = assetStore.PublicURL(assetStore.ObjectKey(id, filename), id, filename)
		width, height := uploadedImageSize(assetPath)
		if width.Valid {
			widthValue = width.Int64
		}
		if height.Valid {
			heightValue = height.Int64
		}
	}
	storageKey := assetStore.ObjectKey(id, filename)
	if err := assetStore.PutFile(c.Request.Context(), storageKey, assetPath, mimeType); err != nil {
		_ = os.Remove(assetPath)
		h.jsonError(c, http.StatusServiceUnavailable, "storage_unavailable", "asset storage is temporarily unavailable")
		return
	}
	now := nowMillis()
	url := assetStore.PublicURL(storageKey, id, filename)
	_, err = h.DB.Exec(
		`INSERT INTO assets (id, owner_user_id, purpose, filename, mime_type, size_bytes, url, thumbnail_url, width, height, storage_key, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, currentUserID(c), purpose, filename, mimeType, sizeBytes, url, thumb, widthValue, heightValue, storageKey, now,
	)
	if err != nil {
		_ = os.Remove(assetPath)
		_ = assetStore.Delete(c.Request.Context(), storageKey)
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "store asset failed")
		return
	}
	if err := h.trackUploadedStickerAsset(id, purpose); err != nil {
		_, _ = h.DB.Exec(`DELETE FROM assets WHERE id = ?`, id)
		_ = assetStore.Delete(c.Request.Context(), storageKey)
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "track sticker asset failed")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"asset": h.assetPayload(id)})
}

func (h *Handler) assetPayload(id string) gin.H {
	var filename, mimeType string
	var width, height sql.NullInt64
	var sizeBytes int64
	var createdAt int64
	err := h.DB.QueryRow(`SELECT filename, size_bytes, mime_type, width, height, created_at FROM assets WHERE id = ?`, id).Scan(&filename, &sizeBytes, &mimeType, &width, &height, &createdAt)
	if err != nil {
		return gin.H{"id": id}
	}
	assetStore := h.assetStore()
	url := assetStore.PublicURL(assetStore.ObjectKey(id, filename), id, filename)
	return gin.H{
		"id":            id,
		"filename":      filename,
		"size_bytes":    sizeBytes,
		"url":           url,
		"thumbnail_url": assetThumbnailURL(url, mimeType),
		"mime_type":     mimeType,
		"width":         nullableInt64(width),
		"height":        nullableInt64(height),
		"created_at":    formatMillis(createdAt),
	}
}

func writeUploadedAsset(filePath string, src io.Reader, maxBytes int64) (int64, string, error) {
	dst, err := os.Create(filePath)
	if err != nil {
		return 0, "", err
	}
	defer dst.Close()

	reader := src
	if maxBytes > 0 {
		reader = &io.LimitedReader{R: src, N: maxBytes + 1}
	}
	var sniff [512]byte
	n, readErr := reader.Read(sniff[:])
	if readErr != nil && readErr != io.EOF {
		return 0, "", readErr
	}
	var written int64
	if n > 0 {
		count, err := dst.Write(sniff[:n])
		if err != nil {
			return 0, "", err
		}
		written += int64(count)
	}
	rest, err := io.Copy(dst, reader)
	if err != nil {
		return 0, "", err
	}
	written += rest
	mimeType := ""
	if n > 0 {
		mimeType = http.DetectContentType(sniff[:n])
	}
	if maxBytes > 0 && written > maxBytes {
		return written, mimeType, errUploadTooLarge
	}
	return written, mimeType, nil
}

func safeAssetFilename(name, mimeType string) string {
	base := path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	ext := safeAssetExtension(path.Ext(base), mimeType)
	stem := strings.TrimSuffix(base, path.Ext(base))
	if stem == "" || stem == "." {
		stem = "file"
	}
	var cleaned strings.Builder
	for _, r := range stem {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			cleaned.WriteRune(r)
		} else {
			cleaned.WriteByte('-')
		}
	}
	value := strings.Trim(cleaned.String(), "-_")
	if value == "" {
		value = "file"
	}
	return value + ext
}

func safeAssetExtension(ext, mimeType string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if len(ext) >= 2 && len(ext) <= 16 {
		valid := true
		for _, r := range ext[1:] {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
				valid = false
				break
			}
		}
		if ext[0] == '.' && valid {
			return ext
		}
	}
	return extensionFromMime(mimeType)
}

func extensionFromMime(mimeType string) string {
	switch canonicalMimeType(mimeType) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	case "application/json":
		return ".json"
	case "application/zip":
		return ".zip"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "video/mp4":
		return ".mp4"
	}
	if extensions, err := mime.ExtensionsByType(canonicalMimeType(mimeType)); err == nil && len(extensions) > 0 {
		return extensions[0]
	}
	return ".bin"
}

func canonicalMimeType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err == nil {
		return strings.ToLower(mediaType)
	}
	mediaType = strings.TrimSpace(strings.Split(value, ";")[0])
	return strings.ToLower(mediaType)
}

func (h *Handler) uploadLimitBytes(image bool) int64 {
	if h != nil && h.Cfg != nil {
		if image && h.Cfg.ImageUploadMaxBytes > 0 {
			return h.Cfg.ImageUploadMaxBytes
		}
		if h.Cfg.AssetUploadMaxBytes > 0 {
			return h.Cfg.AssetUploadMaxBytes
		}
	}
	if image {
		return config.DefaultImageUploadMaxBytes
	}
	return config.DefaultAssetUploadMaxBytes
}

func uploadTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.Is(err, errUploadTooLarge) || errors.As(err, &maxErr) || strings.Contains(err.Error(), "request body too large")
}

func uploadedImageSize(filePath string) (sql.NullInt64, sql.NullInt64) {
	file, err := os.Open(filePath)
	if err != nil {
		return sql.NullInt64{}, sql.NullInt64{}
	}
	defer file.Close()
	cfg, _, err := image.DecodeConfig(file)
	if err != nil {
		return sql.NullInt64{}, sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(cfg.Width), Valid: cfg.Width > 0}, sql.NullInt64{Int64: int64(cfg.Height), Valid: cfg.Height > 0}
}
