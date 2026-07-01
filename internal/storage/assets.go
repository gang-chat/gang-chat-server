package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
)

const defaultAssetCacheControl = "public, max-age=31536000, immutable"
const defaultAssetCacheMaxAgeSeconds int64 = 31536000
const assetObjectPrefix = "assets"

type AssetStorage struct {
	remote remoteStore
}

type remoteStore interface {
	PutFile(ctx context.Context, key, filePath, mimeType, cacheControl string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

func NewAssetStorage(cfg *config.Config) (*AssetStorage, error) {
	remote, err := newS3Remote(cfg)
	if err != nil {
		return nil, err
	}
	return &AssetStorage{remote: remote}, nil
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
		return errors.New("S3 storage is not configured")
	}
	return s.remote.PutFile(ctx, cleanObjectKey(key), filePath, mimeType, s.CacheControl())
}

func (s *AssetStorage) Delete(ctx context.Context, key string) error {
	if s == nil || s.remote == nil {
		return errors.New("S3 storage is not configured")
	}
	return s.remote.Delete(ctx, cleanObjectKey(key))
}

func (s *AssetStorage) Open(ctx context.Context, key, assetID, filename string) (io.ReadCloser, error) {
	if s == nil || s.remote == nil {
		return nil, errors.New("S3 storage is not configured")
	}
	return s.remote.Get(ctx, cleanObjectKey(key))
}

type s3Remote struct {
	client *s3.Client
	bucket string
}

func newS3Remote(cfg *config.Config) (*s3Remote, error) {
	if cfg == nil {
		return nil, errors.New("S3 storage requires config")
	}
	if strings.TrimSpace(cfg.S3Endpoint) == "" {
		return nil, errors.New("S3 storage requires s3_endpoint")
	}
	if strings.TrimSpace(cfg.S3Bucket) == "" {
		return nil, errors.New("S3 storage requires s3_bucket")
	}
	if strings.TrimSpace(cfg.S3AccessKeyID) == "" || strings.TrimSpace(cfg.S3SecretAccessKey) == "" {
		return nil, errors.New("S3 storage requires s3_access_key_id and s3_secret_access_key")
	}
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.S3Endpoint), "/")
	if _, err := url.ParseRequestURI(endpoint); err != nil {
		return nil, fmt.Errorf("parse S3 endpoint: %w", err)
	}
	region := strings.TrimSpace(cfg.S3Region)
	if region == "" {
		region = "us-east-1"
	}
	client := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(endpoint),
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
			strings.TrimSpace(cfg.S3AccessKeyID),
			strings.TrimSpace(cfg.S3SecretAccessKey),
			strings.TrimSpace(cfg.S3SessionToken),
		)),
		UsePathStyle: cfg.S3ForcePathStyle,
	})
	return &s3Remote{
		client: client,
		bucket: strings.TrimSpace(cfg.S3Bucket),
	}, nil
}

func (r *s3Remote) PutFile(ctx context.Context, key, filePath, mimeType, cacheControl string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	input := &s3.PutObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
		Body:   file,
	}
	if mimeType != "" {
		input.ContentType = aws.String(mimeType)
	}
	if cacheControl != "" {
		input.CacheControl = aws.String(cacheControl)
	}
	_, err = r.client.PutObject(ctx, input)
	return err
}

func (r *s3Remote) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := r.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (r *s3Remote) Delete(ctx context.Context, key string) error {
	_, err := r.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	return err
}

type memoryRemote struct {
	mu    sync.Mutex
	files map[string][]byte
}

// NewMemoryAssetStorage builds an in-memory S3-like store for tests.
func NewMemoryAssetStorage() *AssetStorage {
	return &AssetStorage{remote: &memoryRemote{files: map[string][]byte{}}}
}

func (r *memoryRemote) PutFile(ctx context.Context, key, filePath, mimeType, cacheControl string) error {
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[cleanObjectKey(key)] = append([]byte(nil), raw...)
	return nil
}

func (r *memoryRemote) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, ok := r.files[cleanObjectKey(key)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), raw...))), nil
}

func (r *memoryRemote) Delete(ctx context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.files, cleanObjectKey(key))
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
