package musicbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPreparePreviewUsesExistingCacheWithoutUpstream(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(nil, Config{Dir: dir}, nil, nil)
	cacheDir := filepath.Join(dir, "previews")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cacheDir, previewCacheKey("netease", "track-1")+".m4a")
	if err := os.WriteFile(want, []byte("cached-m4a"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyPaths := []string{
		filepath.Join(cacheDir, "legacy.ogg"),
		filepath.Join(cacheDir, "legacy.mp3"),
	}
	for _, legacy := range legacyPaths {
		if err := os.WriteFile(legacy, []byte("legacy-preview"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := m.PreparePreview(context.Background(), "netease", "track-1")
	if err != nil {
		t.Fatalf("PreparePreview returned error: %v", err)
	}
	if got != want {
		t.Fatalf("PreparePreview path = %q, want %q", got, want)
	}
	for _, legacy := range legacyPaths {
		if _, err := os.Stat(legacy); !os.IsNotExist(err) {
			t.Fatalf("legacy preview still exists at %q: %v", legacy, err)
		}
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

func TestPreviewTranscodeArgsUsePortableM4AAAC(t *testing.T) {
	args := previewTranscodeArgs("netease", "https://example.test/source", "preview.m4a")
	wantTokens := []string{"-c:a", "aac", "-movflags", "+faststart", "-f", "mp4"}
	for _, token := range wantTokens {
		if !slices.Contains(args, token) {
			t.Fatalf("preview transcode args do not contain %q: %#v", token, args)
		}
	}
	if slices.Contains(args, "libmp3lame") || slices.Contains(args, "libopus") {
		t.Fatalf("preview transcode unexpectedly uses a non-portable encoder: %#v", args)
	}
}
