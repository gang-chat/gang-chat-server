package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
)

const defaultAssetCacheControl = "public, max-age=31536000, immutable"
const defaultAssetCacheMaxAgeSeconds int64 = 31536000
const assetObjectPrefix = "assets"

type AssetStorage struct {
	remote remoteStore
}

type remoteStore interface {
	Put(key string, body io.Reader) error
	Get(key string) (io.ReadCloser, error)
	Delete(key string) error
}

// NewAssetStorage keeps assets in the storage directory, storage_path.
func NewAssetStorage(cfg *config.Config) (*AssetStorage, error) {
	files, err := NewFiles(cfg.StoragePath)
	if err != nil {
		return nil, err
	}
	return NewFileAssetStorage(files), nil
}

// NewFileAssetStorage keeps assets in files.
func NewFileAssetStorage(files *Files) *AssetStorage {
	return &AssetStorage{remote: fileRemote{files}}
}

func (s *AssetStorage) CacheControl() string {
	return defaultAssetCacheControl
}

func (s *AssetStorage) ApplyCacheHeaders(header http.Header, now time.Time) {
	if header == nil {
		return
	}
	cacheControl := s.CacheControl()
	header.Set("Cache-Control", cacheControl)
	maxAge := cacheMaxAge(cacheControl)
	if maxAge > 0 {
		header.Set("Expires", now.UTC().Add(time.Duration(maxAge)*time.Second).Format(http.TimeFormat))
	}
}

func cacheMaxAge(cacheControl string) int64 {
	for _, part := range strings.Split(cacheControl, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if strings.HasPrefix(part, "max-age=") {
			value := strings.TrimSpace(strings.TrimPrefix(part, "max-age="))
			maxAge, err := strconv.ParseInt(value, 10, 64)
			if err == nil && maxAge > 0 {
				return maxAge
			}
		}
	}
	return 0
}

func (s *AssetStorage) ObjectKey(assetID, filename string) string {
	return path.Join(assetObjectPrefix, assetID, filename)
}

func (s *AssetStorage) PublicURL(key, assetID, filename string) string {
	return "/" + path.Join("assets", assetID, filename)
}

func (s *AssetStorage) PutFile(ctx context.Context, key, filePath, mimeType string) error {
	if s == nil || s.remote == nil {
		return errors.New("asset storage is not configured")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	return s.remote.Put(cleanObjectKey(key), file)
}

func (s *AssetStorage) Delete(ctx context.Context, key string) error {
	if s == nil || s.remote == nil {
		return errors.New("asset storage is not configured")
	}
	return s.remote.Delete(cleanObjectKey(key))
}

// Open reads an asset; a missing one is os.ErrNotExist.
func (s *AssetStorage) Open(ctx context.Context, key, assetID, filename string) (io.ReadCloser, error) {
	if s == nil || s.remote == nil {
		return nil, errors.New("asset storage is not configured")
	}
	return s.remote.Get(cleanObjectKey(key))
}

type fileRemote struct {
	files *Files
}

func (r fileRemote) Put(key string, body io.Reader) error { return r.files.Put(key, body) }

func (r fileRemote) Get(key string) (io.ReadCloser, error) {
	file, _, err := r.files.Open(key)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (r fileRemote) Delete(key string) error { return r.files.Delete(key) }

type memoryRemote struct {
	mu    sync.Mutex
	files map[string][]byte
}

// NewMemoryAssetStorage builds an in-memory store for tests.
func NewMemoryAssetStorage() *AssetStorage {
	return &AssetStorage{remote: &memoryRemote{files: map[string][]byte{}}}
}

func (r *memoryRemote) Put(key string, body io.Reader) error {
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[key] = raw
	return nil
}

func (r *memoryRemote) Get(key string) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, ok := r.files[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), raw...))), nil
}

func (r *memoryRemote) Delete(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.files, key)
	return nil
}

func cleanObjectKey(value string) string {
	cleaned := path.Clean(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"))
	cleaned = strings.Trim(cleaned, "/")
	if cleaned == "." {
		return ""
	}
	return cleaned
}
