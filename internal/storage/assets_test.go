package storage

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
)

func TestNewAssetStorageKeepsAssetsInTheStorageDirectory(t *testing.T) {
	dir := t.TempDir()
	store, err := NewAssetStorage(&config.Config{StoragePath: dir})
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
	source := filepath.Join(t.TempDir(), "upload.png")
	if err := os.WriteFile(source, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.PutFile(context.Background(), key, source, "image/png"); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "assets", "asset_1", "room.png")); err != nil || string(raw) != "png" {
		t.Fatalf("stored file: %q %v", raw, err)
	}
	if err := store.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(context.Background(), key, "asset_1", "room.png"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a deleted asset must read as missing: %v", err)
	}
}

func TestNewAssetStorageRequiresAStoragePath(t *testing.T) {
	_, err := NewAssetStorage(&config.Config{})
	if err == nil || !strings.Contains(err.Error(), "storage_path") {
		t.Fatalf("expected a missing storage_path error, got %v", err)
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
