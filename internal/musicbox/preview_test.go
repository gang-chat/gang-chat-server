package musicbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPreparePreviewUsesExistingCacheWithoutUpstream(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(nil, Config{Dir: dir}, nil, nil)
	cacheDir := filepath.Join(dir, "previews")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cacheDir, previewCacheKey("netease", "track-1")+".ogg")
	if err := os.WriteFile(want, []byte("cached-ogg"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := m.PreparePreview(context.Background(), "netease", "track-1")
	if err != nil {
		t.Fatalf("PreparePreview returned error: %v", err)
	}
	if got != want {
		t.Fatalf("PreparePreview path = %q, want %q", got, want)
	}
}

func TestPreparePreviewRejectsEmptyTrackID(t *testing.T) {
	m := NewManager(nil, Config{Dir: t.TempDir()}, nil, nil)
	if _, err := m.PreparePreview(context.Background(), "netease", "  "); err == nil {
		t.Fatal("PreparePreview should reject an empty track id")
	}
}

func TestPreviewCacheKeySeparatesSourceAndTrack(t *testing.T) {
	a := previewCacheKey("netease", "123")
	b := previewCacheKey("bilibili", "123")
	c := previewCacheKey("netease", "456")
	if a == b || a == c || b == c {
		t.Fatalf("preview cache keys should be distinct: %q %q %q", a, b, c)
	}
}
