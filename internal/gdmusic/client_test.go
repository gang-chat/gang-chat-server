package gdmusic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSearchParsesResultsAndBackfillsSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("types"); got != "search" {
			t.Errorf("types = %q, want search", got)
		}
		if got := r.URL.Query().Get("name"); got != "hello" {
			t.Errorf("name = %q, want hello", got)
		}
		w.Header().Set("Content-Type", "application/json")
		// One row omits source to exercise backfill.
		_, _ = w.Write([]byte(`[
			{"id":"123","name":"Song A","artist":["X","Y"],"album":"Alb","pic_id":"p1","lyric_id":"l1","source":"netease"},
			{"id":"456","name":"Song B","artist":["Z"],"album":"Alb2","pic_id":"p2","lyric_id":"l2"}
		]`))
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithDefaultSource("netease"))
	results, err := c.Search(context.Background(), "", "hello", 0, 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].ID != "123" || results[0].Name != "Song A" || len(results[0].Artists) != 2 {
		t.Errorf("unexpected first result: %+v", results[0])
	}
	if results[1].Source != "netease" {
		t.Errorf("source backfill failed: got %q, want netease", results[1].Source)
	}
}

func TestSearchRequiresKeyword(t *testing.T) {
	c := New()
	if _, err := c.Search(context.Background(), "netease", "  ", 0, 0); err == nil {
		t.Fatal("expected error for empty keyword")
	}
}

func TestTrackURLRejectsEmptyURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("br"); got != "192" {
			t.Errorf("br = %q, want 192 (default)", got)
		}
		_, _ = w.Write([]byte(`{"url":"","br":0,"size":0}`))
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL), WithDefaultBitrate("192"))
	if _, err := c.TrackURL(context.Background(), "netease", "123", ""); err == nil {
		t.Fatal("expected error for empty url")
	}
}

func TestTrackURLReturnsResolved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"url":"https://cdn/song.mp3","br":320,"size":4096}`))
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	got, err := c.TrackURL(context.Background(), "netease", "123", "320")
	if err != nil {
		t.Fatalf("TrackURL: %v", err)
	}
	if got.URL != "https://cdn/song.mp3" || got.Bitrate != 320 || got.SizeKB != 4096 {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestUpstreamErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer srv.Close()

	c := New(WithBaseURL(srv.URL))
	if _, err := c.Search(context.Background(), "netease", "x", 0, 0); err == nil {
		t.Fatal("expected error on non-200 status")
	}
}

func TestRateLimiterBlocksOverLimit(t *testing.T) {
	r := newRateLimiter(2, 100*time.Millisecond)
	ctx := context.Background()
	start := time.Now()
	// First two are immediate.
	_ = r.wait(ctx)
	_ = r.wait(ctx)
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Fatalf("first two waits took %v, expected ~0", elapsed)
	}
	// Third must block until the window slides (~100ms).
	_ = r.wait(ctx)
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("third wait returned after %v, expected it to block ~100ms", elapsed)
	}
}

func TestRateLimiterRespectsContext(t *testing.T) {
	r := newRateLimiter(1, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_ = r.wait(context.Background()) // consume the only slot
	if err := r.wait(ctx); err == nil {
		t.Fatal("expected context deadline error")
	}
}
