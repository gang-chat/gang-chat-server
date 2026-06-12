package qqmusic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer builds a fake QQ音乐 service. It tracks login count and gates
// protected endpoints on the sid cookie, so tests can exercise the auto-relogin
// path by clearing the session.
type fakeService struct {
	password   string
	loggedIn   bool
	loginCalls int
}

func (f *fakeService) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		f.loginCalls++
		var body struct {
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Password != f.password {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false}`))
			return
		}
		f.loggedIn = true
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "session-token", Path: "/", HttpOnly: true})
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	guard := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie("sid")
			if !f.loggedIn || err != nil || c.Value != "session-token" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("/api/search", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "" {
			_, _ = w.Write([]byte(`{"total":0,"page":1,"songs":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"total":2,"page":1,"songs":[
			{"mid":"abc","name":"晴天","singer":"周杰伦","album":"叶惠美"},
			{"mid":"def","name":"合唱","singer":"歌手一、歌手二"}
		]}`))
	}))
	mux.HandleFunc("/api/url", guard(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("mid") {
		case "abc":
			_, _ = w.Write([]byte(`{"ogg_url":"http://cdn.example/abc.ogg?vkey=x","expired":false}`))
		case "noright":
			_, _ = w.Write([]byte(`{"ogg_url":null,"expired":false}`))
		case "stale":
			_, _ = w.Write([]byte(`{"ogg_url":null,"expired":true}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"缺少 mid"}`))
		}
	}))
	return mux
}

func newClient(t *testing.T, f *fakeService) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	c, err := New(srv.URL, f.password)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func TestLoginAndSearch(t *testing.T) {
	f := &fakeService{password: "secret"}
	c, srv := newClient(t, f)
	defer srv.Close()

	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	res, err := c.Search(context.Background(), "周杰伦", 20, 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d", len(res))
	}
	if res[0].ID != "abc" || res[0].Name != "晴天" || res[0].Album != "叶惠美" {
		t.Errorf("unexpected first result: %+v", res[0])
	}
	if len(res[0].Artists) != 1 || res[0].Artists[0] != "周杰伦" {
		t.Errorf("unexpected artists: %v", res[0].Artists)
	}
	if len(res[1].Artists) != 2 {
		t.Errorf("want 2 singers split, got %v", res[1].Artists)
	}
}

func TestWrongPassword(t *testing.T) {
	f := &fakeService{password: "secret"}
	c, srv := newClient(t, f)
	defer srv.Close()
	c.password = "wrong"
	err := c.Login(context.Background())
	if err == nil || !strings.Contains(err.Error(), "wrong service password") {
		t.Fatalf("want wrong-password error, got %v", err)
	}
}

func TestAutoReloginOn401(t *testing.T) {
	f := &fakeService{password: "secret"}
	c, srv := newClient(t, f)
	defer srv.Close()

	// First call with no session: getJSON should hit 401, auto-login, retry.
	res, err := c.Search(context.Background(), "周杰伦", 20, 1)
	if err != nil {
		t.Fatalf("Search with auto-login: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d", len(res))
	}
	if f.loginCalls != 1 {
		t.Errorf("want exactly 1 auto-login, got %d", f.loginCalls)
	}

	// Simulate the session expiring server-side; next call should re-login once.
	f.loggedIn = false
	if _, err := c.TrackURL(context.Background(), "abc"); err != nil {
		t.Fatalf("TrackURL after session reset: %v", err)
	}
	if f.loginCalls != 2 {
		t.Errorf("want 2 logins after session reset, got %d", f.loginCalls)
	}
}

func TestTrackURL(t *testing.T) {
	f := &fakeService{password: "secret"}
	c, srv := newClient(t, f)
	defer srv.Close()
	_ = c.Login(context.Background())

	url, err := c.TrackURL(context.Background(), "abc")
	if err != nil {
		t.Fatalf("TrackURL: %v", err)
	}
	if !strings.HasPrefix(url, "http://cdn.example/abc.ogg") {
		t.Errorf("unexpected url: %s", url)
	}

	if _, err := c.TrackURL(context.Background(), "noright"); err == nil ||
		!strings.Contains(err.Error(), "lacks rights") {
		t.Errorf("want lacks-rights error, got %v", err)
	}
	if _, err := c.TrackURL(context.Background(), "stale"); err == nil ||
		!strings.Contains(err.Error(), "expired") {
		t.Errorf("want expired error, got %v", err)
	}
	if _, err := c.TrackURL(context.Background(), ""); err == nil {
		t.Errorf("want error for empty mid")
	}
}
