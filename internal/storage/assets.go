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

	"github.com/tencentyun/cos-go-sdk-v5"
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
		if backend == "" && hasCOSConfig(cfg) {
			backend = "cos"
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
	case "cos", "tencent-cos", "tencent_cloud_cos":
		remote, err := newCOSRemote(cfg)
		if err != nil {
			return nil, err
		}
		store.remote = remote
		if store.publicBase == "" {
			store.publicBase = remote.publicBase
		}
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

type cosRemote struct {
	client     *cos.Client
	publicBase string
	objectACL  string
}

func newCOSRemote(cfg *config.Config) (*cosRemote, error) {
	if cfg == nil {
		return nil, errors.New("COS storage requires config")
	}
	if cfg.COSSecretID == "" || cfg.COSSecretKey == "" {
		return nil, errors.New("COS storage requires GANG_COS_SECRET_ID and GANG_COS_SECRET_KEY")
	}
	bucketURL, err := cosBucketURL(cfg)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(bucketURL)
	if err != nil {
		return nil, fmt.Errorf("parse COS bucket URL: %w", err)
	}
	client := cos.NewClient(
		&cos.BaseURL{BucketURL: u},
		&http.Client{
			Transport: &cos.AuthorizationTransport{
				SecretID:     cfg.COSSecretID,
				SecretKey:    cfg.COSSecretKey,
				SessionToken: cfg.COSSessionToken,
			},
		},
	)
	return &cosRemote{
		client:     client,
		publicBase: strings.TrimRight(bucketURL, "/"),
		objectACL:  strings.TrimSpace(cfg.COSObjectACL),
	}, nil
}

func (r *cosRemote) PutFile(ctx context.Context, key, filePath, mimeType, cacheControl string) error {
	options := &cos.ObjectPutOptions{
		ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{
			ContentType:  mimeType,
			CacheControl: cacheControl,
		},
	}
	if r.objectACL != "" {
		options.ACLHeaderOptions = &cos.ACLHeaderOptions{XCosACL: r.objectACL}
	}
	_, err := r.client.Object.PutFromFile(ctx, key, filePath, options)
	return err
}

func (r *cosRemote) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := r.client.Object.Get(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (r *cosRemote) Delete(ctx context.Context, key string) error {
	_, err := r.client.Object.Delete(ctx, key)
	return err
}

func cosBucketURL(cfg *config.Config) (string, error) {
	bucketURL := strings.TrimSpace(cfg.COSBucketURL)
	if bucketURL == "" {
		if cfg.COSBucket == "" || cfg.COSRegion == "" {
			return "", errors.New("COS storage requires GANG_COS_BUCKET and GANG_COS_REGION, or GANG_COS_BUCKET_URL")
		}
		bucketURL = fmt.Sprintf("https://%s.cos.%s.myqcloud.com", cfg.COSBucket, cfg.COSRegion)
	}
	return strings.TrimRight(bucketURL, "/"), nil
}

func hasCOSConfig(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	return strings.TrimSpace(cfg.COSBucketURL) != "" ||
		strings.TrimSpace(cfg.COSBucket) != "" ||
		strings.TrimSpace(cfg.COSRegion) != "" ||
		strings.TrimSpace(cfg.COSSecretID) != "" ||
		strings.TrimSpace(cfg.COSSecretKey) != "" ||
		strings.TrimSpace(cfg.COSSessionToken) != ""
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
