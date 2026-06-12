package meloyou

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zhuangkaiyi/gang-chat/server/internal/gdmusic"
)

func TestKuwoSearchParsesRids(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("all"); got != "我爱你" {
			t.Errorf("all = %q, want 我爱你", got)
		}
		_, _ = w.Write([]byte(`{"abslist":[
			{"DC_TARGETID":"85671606","NAME":"我爱你","ARTIST":"王菲"},
			{"DC_TARGETID":"387522","NAME":"我爱你","ARTIST":"卢广仲&朋友"}
		]}`))
	}))
	defer srv.Close()

	r := New(gdmusic.New(), WithKuwoEndpoints(srv.URL, nil))
	tracks, err := r.Search(context.Background(), "kuwo", "我爱你", 0, 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks, want 2", len(tracks))
	}
	if tracks[0].ID != "85671606" || tracks[0].Name != "我爱你" || tracks[0].Source != "kuwo" {
		t.Errorf("unexpected first track: %+v", tracks[0])
	}
	if len(tracks[1].Artists) != 2 || tracks[1].Artists[0] != "卢广仲" {
		t.Errorf("artist split failed: %+v", tracks[1].Artists)
	}
}

func TestKuwoTrackURLFallsThroughEndpoints(t *testing.T) {
	// First endpoint returns an empty url, second a real one; resolve should
	// skip the first and succeed on the second.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"data":{"url":""}}`))
	}))
	defer empty.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("id"); got != "85671606" {
			t.Errorf("id = %q, want 85671606", got)
		}
		_, _ = w.Write([]byte(`{"code":200,"data":{"url":"https://cdn.example/song.mp3"}}`))
	}))
	defer good.Close()

	r := New(gdmusic.New(), WithKuwoEndpoints("https://search.example", []string{
		empty.URL + "?id={id}",
		good.URL + "?id={id}",
	}))
	res, err := r.TrackURL(context.Background(), "kuwo", "85671606", "320")
	if err != nil {
		t.Fatalf("TrackURL: %v", err)
	}
	if res.URL != "https://cdn.example/song.mp3" {
		t.Errorf("url = %q, want the good endpoint's url", res.URL)
	}
}

func TestKuwoTrackURLErrorsWhenAllEmpty(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"data":{"url":""}}`))
	}))
	defer empty.Close()

	r := New(gdmusic.New(), WithKuwoEndpoints("https://search.example", []string{empty.URL + "?id={id}"}))
	if _, err := r.TrackURL(context.Background(), "kuwo", "1", ""); err == nil {
		t.Fatal("expected error when every endpoint returns an empty url")
	}
}

func TestQQSearchParsesMids(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Referer"); got != "https://y.qq.com" {
			t.Errorf("Referer = %q, want https://y.qq.com", got)
		}
		_, _ = w.Write([]byte(`{"req_0":{"code":0,"data":{"body":{"song":{"list":[
			{"mid":"00476zUy2oLuhh","name":"我爱你","singer":[{"name":"王菲"}]}
		]}}}}}`))
	}))
	defer srv.Close()

	r := New(gdmusic.New(), WithQQEndpoints(srv.URL, "https://resolve.example?mid={mid}"))
	tracks, err := r.Search(context.Background(), "tencent", "我爱你", 0, 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(tracks) != 1 || tracks[0].ID != "00476zUy2oLuhh" || tracks[0].Source != "tencent" {
		t.Fatalf("unexpected tracks: %+v", tracks)
	}
	if len(tracks[0].Artists) != 1 || tracks[0].Artists[0] != "王菲" {
		t.Errorf("artist parse failed: %+v", tracks[0].Artists)
	}
}

func TestQQTrackURLResolves(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("mid"); got != "00476zUy2oLuhh" {
			t.Errorf("mid = %q, want 00476zUy2oLuhh", got)
		}
		_, _ = w.Write([]byte(`{"name":"我爱你","url":"https://cdn.qq/song.m4a"}`))
	}))
	defer srv.Close()

	r := New(gdmusic.New(), WithQQEndpoints("https://search.example", srv.URL+"?mid={mid}"))
	res, err := r.TrackURL(context.Background(), "tencent", "00476zUy2oLuhh", "")
	if err != nil {
		t.Fatalf("TrackURL: %v", err)
	}
	if res.URL != "https://cdn.qq/song.m4a" {
		t.Errorf("url = %q", res.URL)
	}
}

func TestPassthroughUsesGDClient(t *testing.T) {
	var gotTypes string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTypes = r.URL.Query().Get("types")
		_, _ = w.Write([]byte(`[{"id":"1","name":"A","artist":["X"],"source":"netease"}]`))
	}))
	defer srv.Close()

	gd := gdmusic.New(gdmusic.WithBaseURL(srv.URL), gdmusic.WithDefaultSource("netease"))
	r := New(gd)
	tracks, err := r.Search(context.Background(), "netease", "hi", 0, 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotTypes != "search" {
		t.Errorf("GD client not used: types=%q", gotTypes)
	}
	if len(tracks) != 1 || tracks[0].Source != "netease" {
		t.Errorf("unexpected passthrough tracks: %+v", tracks)
	}
}

func TestSearchRequiresKeyword(t *testing.T) {
	r := New(gdmusic.New())
	if _, err := r.Search(context.Background(), "kuwo", "  ", 0, 0); err == nil {
		t.Fatal("expected error for empty keyword")
	}
}
