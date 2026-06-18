package chat

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	"github.com/zhuangkaiyi/gang-chat/server/internal/storage"
)

func RegisterAssetRoutes(r gin.IRouter, db *sql.DB, cfg *config.Config, assetStores ...*storage.AssetStorage) {
	assetStore := firstAssetStore(assetStores)
	if assetStore == nil {
		var err error
		assetStore, err = storage.NewAssetStorage(cfg)
		if err != nil {
			panic(err)
		}
	}
	handler := func(c *gin.Context) {
		assetID := c.Param("asset_id")
		filename := c.Param("filename")
		metadata, err := assetRouteMetadata(db, assetStore, assetID, filename)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": "not_found", "message": "asset not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error", "message": "read asset metadata failed"})
			return
		}
		applyAssetCacheHeaders(c, assetStore, metadata)
		if assetNotModified(c, metadata) {
			c.Status(http.StatusNotModified)
			return
		}
		if assetStore.HasPublicBase() {
			c.Redirect(http.StatusFound, assetStore.PublicURL(metadata.storageKey, assetID, filename))
			return
		}
		body, err := assetStore.Open(c.Request.Context(), metadata.storageKey, assetID, filename)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				c.JSON(http.StatusNotFound, gin.H{"error": "not_found", "message": "asset not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error", "message": "read asset failed"})
			return
		}
		defer body.Close()
		if metadata.mimeType != "" {
			c.Header("Content-Type", metadata.mimeType)
		}
		if c.Request.Method == http.MethodHead {
			c.Status(http.StatusOK)
			return
		}
		contentType := metadata.mimeType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		c.DataFromReader(http.StatusOK, -1, contentType, body, nil)
	}
	r.GET("/assets/:asset_id/:filename", handler)
	r.HEAD("/assets/:asset_id/:filename", handler)
}

type assetRouteMetadataResult struct {
	storageKey string
	mimeType   string
	sizeBytes  int64
	createdAt  int64
}

func assetRouteMetadata(db *sql.DB, assetStore *storage.AssetStorage, assetID, filename string) (assetRouteMetadataResult, error) {
	var storageKey sql.NullString
	var mimeType sql.NullString
	var meta assetRouteMetadataResult
	err := db.QueryRow(`SELECT storage_key, mime_type, size_bytes, created_at FROM assets WHERE id = ? AND filename = ?`, assetID, filename).Scan(&storageKey, &mimeType, &meta.sizeBytes, &meta.createdAt)
	if err != nil {
		return assetRouteMetadataResult{}, err
	}
	if storageKey.Valid {
		meta.storageKey = storageKey.String
	}
	if meta.storageKey == "" {
		meta.storageKey = assetStore.ObjectKey(assetID, filename)
	}
	if mimeType.Valid {
		meta.mimeType = mimeType.String
	}
	return meta, nil
}

func applyAssetCacheHeaders(c *gin.Context, assetStore *storage.AssetStorage, metadata assetRouteMetadataResult) {
	assetStore.ApplyCacheHeaders(c.Writer.Header(), time.Now())
	if metadata.mimeType != "" {
		c.Header("Content-Type", metadata.mimeType)
	}
	if etag := assetETag(c.Param("asset_id"), metadata); etag != "" {
		c.Header("ETag", etag)
	}
	if lastModified := assetLastModified(metadata); !lastModified.IsZero() {
		c.Header("Last-Modified", lastModified.Format(http.TimeFormat))
	}
}

func assetNotModified(c *gin.Context, metadata assetRouteMetadataResult) bool {
	etag := assetETag(c.Param("asset_id"), metadata)
	if etag != "" {
		for _, item := range strings.Split(c.GetHeader("If-None-Match"), ",") {
			item = strings.TrimSpace(item)
			if item == "*" || item == etag {
				return true
			}
		}
	}
	lastModified := assetLastModified(metadata)
	if lastModified.IsZero() {
		return false
	}
	if raw := c.GetHeader("If-Modified-Since"); raw != "" {
		if since, err := http.ParseTime(raw); err == nil && !lastModified.After(since) {
			return true
		}
	}
	return false
}

func assetETag(assetID string, metadata assetRouteMetadataResult) string {
	if assetID == "" || metadata.createdAt <= 0 {
		return ""
	}
	return `"` + assetID + `-` + strconv.FormatInt(metadata.createdAt, 16) + `-` + strconv.FormatInt(metadata.sizeBytes, 16) + `"`
}

func assetLastModified(metadata assetRouteMetadataResult) time.Time {
	if metadata.createdAt <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(metadata.createdAt).UTC().Truncate(time.Second)
}

func (h *Handler) assetStore() *storage.AssetStorage {
	if h != nil && h.Assets != nil {
		return h.Assets
	}
	store, err := storage.NewAssetStorage(h.Cfg)
	if err != nil {
		panic(err)
	}
	return store
}
