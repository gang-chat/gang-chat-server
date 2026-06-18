package storage

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
)

func TestNewAssetStorageAutoEnablesCOSWhenCOSConfigIsPresent(t *testing.T) {
	store, err := NewAssetStorage(&config.Config{
		COSBucket:         "gang-chat-1234567890",
		COSRegion:         "ap-shanghai",
		COSSecretID:       "sid",
		COSSecretKey:      "skey",
		AssetObjectPrefix: "room-assets",
	})
	if err != nil {
		t.Fatalf("NewAssetStorage returned error: %v", err)
	}
	if !store.RemoteEnabled() {
		t.Fatalf("COS config should enable remote storage")
	}

	key := store.ObjectKey("asset_1", "room.png")
	if key != "room-assets/asset_1/room.png" {
		t.Fatalf("unexpected object key: %q", key)
	}
	if got := store.PublicURL(key, "asset_1", "room.png"); got != "https://gang-chat-1234567890.cos.ap-shanghai.myqcloud.com/room-assets/asset_1/room.png" {
		t.Fatalf("unexpected COS public URL: %q", got)
	}
}

func TestNewAssetStorageRespectsExplicitLocalBackend(t *testing.T) {
	store, err := NewAssetStorage(&config.Config{
		StorageBackend: "local",
		COSBucket:      "gang-chat-1234567890",
		COSRegion:      "ap-shanghai",
		COSSecretID:    "sid",
		COSSecretKey:   "skey",
	})
	if err != nil {
		t.Fatalf("NewAssetStorage returned error: %v", err)
	}
	if store.RemoteEnabled() {
		t.Fatalf("explicit local backend should not enable COS")
	}
}

func TestNewAssetStorageReportsIncompleteAutoCOSConfig(t *testing.T) {
	_, err := NewAssetStorage(&config.Config{
		COSBucket: "gang-chat-1234567890",
		COSRegion: "ap-shanghai",
	})
	if err == nil {
		t.Fatalf("expected missing COS credentials error")
	}
	if !strings.Contains(err.Error(), "GANG_COS_SECRET_ID") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAssetStorageBuildsExpiringCacheHeadersFromTTL(t *testing.T) {
	store, err := NewAssetStorage(&config.Config{
		StorageBackend:       "local",
		AssetCacheTTLSeconds: 60,
		AssetObjectPrefix:    "assets",
	})
	if err != nil {
		t.Fatalf("NewAssetStorage returned error: %v", err)
	}

	headers := http.Header{}
	now := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)
	store.ApplyCacheHeaders(headers, now)

	if got := headers.Get("Cache-Control"); got != "public, max-age=60, immutable" {
		t.Fatalf("unexpected Cache-Control: %q", got)
	}
	if got := headers.Get("Expires"); got != "Thu, 18 Jun 2026 10:01:00 GMT" {
		t.Fatalf("unexpected Expires: %q", got)
	}
}

func TestAssetStorageHonorsExplicitCacheControl(t *testing.T) {
	store, err := NewAssetStorage(&config.Config{
		StorageBackend:    "local",
		AssetCacheControl: "private, max-age=5",
	})
	if err != nil {
		t.Fatalf("NewAssetStorage returned error: %v", err)
	}

	headers := http.Header{}
	store.ApplyCacheHeaders(headers, time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC))

	if got := headers.Get("Cache-Control"); got != "private, max-age=5" {
		t.Fatalf("unexpected Cache-Control: %q", got)
	}
	if got := headers.Get("Expires"); got != "Thu, 18 Jun 2026 10:00:05 GMT" {
		t.Fatalf("unexpected Expires: %q", got)
	}
}
