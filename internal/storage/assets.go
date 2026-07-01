package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
)

const defaultAssetCacheControl = "public, max-age=31536000, immutable"
const defaultAssetCacheMaxAgeSeconds int64 = 31536000

type AssetStorage struct {
	cacheDir     string
	objectPrefix string
	publicBase   string
	cacheControl string
	remote       remoteStore
}

type remoteStore interface {
	PutFile(ctx context.Context, key, filePath, mimeType, cacheControl string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

func NewAssetStorage(cfg *config.Config) (*AssetStorage, error) {
	cacheDir := "assets"
	backend := ""
	objectPrefix := "assets"
	publicBase := ""
	cacheControl := ""
	cacheMaxAgeSeconds := defaultAssetCacheMaxAgeSeconds
	if cfg != nil {
		if cfg.AssetDir != "" {
			cacheDir = cfg.AssetDir
		}
		if cfg.StorageBackend != "" {
			backend = strings.ToLower(strings.TrimSpace(cfg.StorageBackend))
		}
		if backend == "" && hasS3Config(cfg) {
			backend = "s3"
		}
		if cfg.AssetObjectPrefix != "" {
			objectPrefix = cfg.AssetObjectPrefix
		}
		publicBase = strings.TrimRight(strings.TrimSpace(cfg.AssetPublicBaseURL), "/")
		if cfg.AssetCacheControl != "" {
			cacheControl = strings.TrimSpace(cfg.AssetCacheControl)
		}
		if cfg.AssetCacheTTLSeconds > 0 {
			cacheMaxAgeSeconds = cfg.AssetCacheTTLSeconds
		}
		if cacheControl == "" {
			cacheControl = assetCacheControl(cacheMaxAgeSeconds)
		}
	}

	store := &AssetStorage{
		cacheDir:     cacheDir,
		objectPrefix: cleanObjectKey(objectPrefix),
		publicBase:   publicBase,
		cacheControl: cacheControl,
	}
	switch backend {
	case "", "local", "disk":
		return store, nil
	case "s3", "s3-compatible", "s3_compatible":
		remote, err := newS3Remote(cfg)
		if err != nil {
			return nil, err
		}
		store.remote = remote
		return store, nil
	default:
		return nil, fmt.Errorf("unsupported storage backend %q", backend)
	}
}

func (s *AssetStorage) CacheControl() string {
	if s == nil || s.cacheControl == "" {
		return defaultAssetCacheControl
	}
	return s.cacheControl
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

func assetCacheControl(maxAgeSeconds int64) string {
	if maxAgeSeconds <= 0 {
		maxAgeSeconds = defaultAssetCacheMaxAgeSeconds
	}
	return "public, max-age=" + strconv.FormatInt(maxAgeSeconds, 10) + ", immutable"
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

func (s *AssetStorage) RemoteEnabled() bool {
	return s != nil && s.remote != nil
}

func (s *AssetStorage) HasPublicBase() bool {
	return s != nil && s.publicBase != ""
}

func (s *AssetStorage) ObjectKey(assetID, filename string) string {
	parts := make([]string, 0, 3)
	if s != nil && s.objectPrefix != "" {
		parts = append(parts, s.objectPrefix)
	}
	parts = append(parts, assetID, filename)
	return path.Join(parts...)
}

func (s *AssetStorage) PublicURL(key, assetID, filename string) string {
	if s != nil && s.publicBase != "" {
		return s.publicBase + "/" + escapeObjectKey(cleanObjectKey(key))
	}
	return "/" + path.Join("assets", assetID, filename)
}

func (s *AssetStorage) CachePath(assetID, filename string) (string, error) {
	if assetID == "" || filename == "" {
		return "", errors.New("asset id and filename are required")
	}
	if strings.Contains(assetID, "/") || strings.Contains(assetID, "\\") || strings.Contains(filename, "/") || strings.Contains(filename, "\\") {
		return "", errors.New("asset path must not contain separators")
	}
	cacheDir := "assets"
	if s != nil && s.cacheDir != "" {
		cacheDir = s.cacheDir
	}
	return filepath.Join(cacheDir, assetID, filename), nil
}

func (s *AssetStorage) PutFile(ctx context.Context, key, filePath, mimeType string) error {
	if s == nil || s.remote == nil {
		return nil
	}
	return s.remote.PutFile(ctx, cleanObjectKey(key), filePath, mimeType, s.CacheControl())
}

func (s *AssetStorage) Delete(ctx context.Context, key string) error {
	if s == nil || s.remote == nil {
		return nil
	}
	return s.remote.Delete(ctx, cleanObjectKey(key))
}

func (s *AssetStorage) Open(ctx context.Context, key, assetID, filename string) (io.ReadCloser, error) {
	if s != nil && s.remote != nil {
		return s.remote.Get(ctx, cleanObjectKey(key))
	}
	localPath, err := s.CachePath(assetID, filename)
	if err != nil {
		return nil, err
	}
	return os.Open(localPath)
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
		return nil, errors.New("S3 storage requires GANG_S3_ENDPOINT")
	}
	if strings.TrimSpace(cfg.S3Bucket) == "" {
		return nil, errors.New("S3 storage requires GANG_S3_BUCKET")
	}
	if strings.TrimSpace(cfg.S3AccessKeyID) == "" || strings.TrimSpace(cfg.S3SecretAccessKey) == "" {
		return nil, errors.New("S3 storage requires GANG_S3_ACCESS_KEY_ID and GANG_S3_SECRET_ACCESS_KEY")
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

func hasS3Config(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	return strings.TrimSpace(cfg.S3Endpoint) != "" ||
		strings.TrimSpace(cfg.S3Bucket) != "" ||
		strings.TrimSpace(cfg.S3AccessKeyID) != "" ||
		strings.TrimSpace(cfg.S3SecretAccessKey) != "" ||
		strings.TrimSpace(cfg.S3SessionToken) != ""
}

func cleanObjectKey(value string) string {
	cleaned := path.Clean(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"))
	cleaned = strings.Trim(cleaned, "/")
	if cleaned == "." {
		return ""
	}
	return cleaned
}

func escapeObjectKey(key string) string {
	parts := strings.Split(cleanObjectKey(key), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
