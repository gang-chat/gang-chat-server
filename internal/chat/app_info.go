package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
)

const latestClientVersion = "0.3.1"

type appVersionCache struct {
	mu        sync.Mutex
	value     gin.H
	expiresAt time.Time
}

func (h *Handler) getAppVersion(c *gin.Context) {
	c.JSON(http.StatusOK, h.resolveAppVersion(c.Request.Context()))
}

func (h *Handler) resolveAppVersion(ctx context.Context) gin.H {
	fallback := fallbackAppVersionInfo(h.Cfg)
	if h.Cfg == nil || !h.Cfg.HasAppVersionManifest() {
		return fallback
	}
	if cached, ok := h.AppInfo.read(); ok {
		return cached
	}
	manifest, err := h.loadAppVersionManifest(ctx)
	if err != nil {
		return fallback
	}
	info := normalizeAppVersionManifest(manifest, fallback)
	h.AppInfo.write(info, time.Duration(h.Cfg.AppVersionCacheTTLSeconds)*time.Second)
	return info
}

func (c *appVersionCache) read() (gin.H, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value == nil || time.Now().After(c.expiresAt) {
		return nil, false
	}
	return cloneGinH(c.value), true
}

func (c *appVersionCache) write(value gin.H, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	c.value = cloneGinH(value)
	c.expiresAt = time.Now().Add(ttl)
}

func (h *Handler) loadAppVersionManifest(ctx context.Context) (map[string]any, error) {
	if h.Cfg.AppVersionManifestPath != "" {
		raw, err := os.ReadFile(h.Cfg.AppVersionManifestPath)
		if err != nil {
			return nil, err
		}
		return decodeAppVersionManifest(raw)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.Cfg.AppVersionManifestURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("app version manifest returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return decodeAppVersionManifest(raw)
}

func decodeAppVersionManifest(raw []byte) (map[string]any, error) {
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func fallbackAppVersionInfo(cfg *config.Config) gin.H {
	latest := latestClientVersion
	minimum := latestClientVersion
	if cfg != nil {
		if value := strings.TrimSpace(cfg.AppLatestVersion); value != "" {
			latest = value
		}
		if value := strings.TrimSpace(cfg.AppMinimumSupportedVersion); value != "" {
			minimum = value
		} else {
			minimum = latest
		}
	}
	return gin.H{
		"version":                   latest,
		"latest_version":            latest,
		"minimum_supported_version": minimum,
	}
}

func normalizeAppVersionManifest(raw map[string]any, fallback gin.H) gin.H {
	info := cloneMapToGinH(raw)
	latest := firstString(raw, "latest_version", "version")
	if latest == "" {
		latest = stringValue(fallback["latest_version"])
	}
	if latest == "" {
		latest = latestClientVersion
	}

	minimum := firstString(raw, "minimum_supported_version", "minimum_version")
	if minimum == "" {
		minimum = stringValue(fallback["minimum_supported_version"])
	}
	if minimum == "" {
		minimum = latest
	}

	info["version"] = latest
	info["latest_version"] = latest
	info["minimum_supported_version"] = minimum

	if firstString(info, "download_url") == "" {
		if url := firstPlatformAssetString(raw, "url"); url != "" {
			info["download_url"] = url
		}
	}
	if firstString(info, "sha256", "checksum") == "" {
		if sha256 := firstPlatformAssetString(raw, "sha256"); sha256 != "" {
			info["sha256"] = sha256
		}
	}
	return info
}

func firstPlatformAssetString(raw map[string]any, field string) string {
	platforms, ok := raw["platforms"].(map[string]any)
	if !ok {
		return ""
	}
	for _, platformKey := range []string{"windows-x64", "windows_x64", "windows"} {
		platform, ok := platforms[platformKey].(map[string]any)
		if !ok {
			continue
		}
		for _, assetKey := range []string{"installer", "setup", "zip", "dmg"} {
			asset, ok := platform[assetKey].(map[string]any)
			if !ok {
				continue
			}
			if value := firstString(asset, field); value != "" {
				return value
			}
		}
	}
	return ""
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(stringValue(values[key])); value != "" {
			return value
		}
	}
	return ""
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return ""
	}
}

func cloneMapToGinH(values map[string]any) gin.H {
	out := gin.H{}
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneGinH(values gin.H) gin.H {
	out := gin.H{}
	for key, value := range values {
		out[key] = value
	}
	return out
}
