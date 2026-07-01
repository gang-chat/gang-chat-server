package storage

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
)

func testS3Config() *config.Config {
	return &config.Config{
		S3Endpoint:        "https://os.ky-z.com:9000",
		S3Bucket:          "gang-chat",
		S3Region:          "us-east-1",
		S3AccessKeyID:     "sid",
		S3SecretAccessKey: "skey",
		S3ForcePathStyle:  true,
	}
}

func TestNewAssetStorageRequiresS3Config(t *testing.T) {
	store, err := NewAssetStorage(testS3Config())
	if err != nil {
		t.Fatalf("NewAssetStorage returned error: %v", err)
	}

	key := store.ObjectKey("asset_1", "room.png")
	if key != "assets/asset_1/room.png" {
		t.Fatalf("unexpected object key: %q", key)
	}
	if got := store.PublicURL(key, "asset_1", "room.png"); got != "/assets/asset_1/room.png" {
		t.Fatalf("unexpected asset URL: %q", got)
	}
}

func TestNewAssetStorageReportsIncompleteS3Config(t *testing.T) {
	_, err := NewAssetStorage(&config.Config{
		S3Endpoint: "https://os.ky-z.com:9000",
		S3Bucket:   "gang-chat",
	})
	if err == nil {
		t.Fatalf("expected missing S3 credentials error")
	}
	if !strings.Contains(err.Error(), "s3_access_key_id") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAssetStorageAppliesFixedCacheHeaders(t *testing.T) {
	store := NewMemoryAssetStorage()
	headers := http.Header{}
	now := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)
	store.ApplyCacheHeaders(headers, now)

	if got := headers.Get("Cache-Control"); got != defaultAssetCacheControl {
		t.Fatalf("unexpected Cache-Control: %q", got)
	}
	if got := headers.Get("Expires"); got != "Fri, 18 Jun 2027 10:00:00 GMT" {
		t.Fatalf("unexpected Expires: %q", got)
	}
}
