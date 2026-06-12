// Package qqmusic is a thin client for the self-hosted QQ音乐 API service
// (the password-gated service documented at deploy time). It covers the two
// endpoints the room music box needs: keyword search (returning stable song
// mids) and on-demand URL resolution (mid -> a short-lived OGG_192 CDN link).
//
// Auth is two-layered on the service side; this client only handles the first
// layer (the service password -> sid cookie). The QQ音乐 account login itself
// (QR scan) is managed out-of-band by the operator. A resolve simply returns a
// null URL when the account lacks rights for a track, which we surface as an
// error to the caller.
package qqmusic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client talks to the QQ音乐 API service. The zero value is not usable; use New.
type Client struct {
	httpClient *http.Client
	baseURL    string // e.g. http://103.47.83.189:12345, no trailing slash
	password   string

	// loginMu serializes (re)login so a burst of 401s triggers a single
	// re-auth rather than a stampede.
	loginMu sync.Mutex
}

// New builds a Client with its own cookie jar (to hold the sid session cookie)
// and a 20s timeout. baseURL must include scheme and host; a trailing slash is
// trimmed.
func New(baseURL, password string) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("qqmusic: cookie jar: %w", err)
	}
	return &Client{
		httpClient: &http.Client{Timeout: 20 * time.Second, Jar: jar},
		baseURL:    strings.TrimRight(baseURL, "/"),
		password:   password,
	}, nil
}

// SearchResult is one track from a search response. ID is the QQ song mid,
// used later to resolve a playable URL.
type SearchResult struct {
	ID      string
	Name    string
	Artists []string
	Album   string
}

// Login exchanges the service password for an sid session cookie, stored in the
// client's jar. It's called automatically on demand, but the caller invokes it
// once at startup as a health check: a failure here means the service is
// unreachable or the password is wrong.
func (c *Client) Login(ctx context.Context) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.loginLocked(ctx)
}

func (c *Client) loginLocked(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"password": c.password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("qqmusic: build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("qqmusic: login request: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("qqmusic: login rejected (wrong service password)")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qqmusic: login failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

// Search queries the service by keyword. count defaults to 20 (clamped 1..50
// by the service); page defaults to 1.
func (c *Client) Search(ctx context.Context, keyword string, count, page int) ([]SearchResult, error) {
	if strings.TrimSpace(keyword) == "" {
		return nil, fmt.Errorf("qqmusic: keyword is required")
	}
	if count <= 0 {
		count = 20
	}
	if page <= 0 {
		page = 1
	}
	q := url.Values{}
	q.Set("q", keyword)
	q.Set("num", strconv.Itoa(count))
	q.Set("page", strconv.Itoa(page))

	var out struct {
		Total int `json:"total"`
		Page  int `json:"page"`
		Songs []struct {
			Mid    string `json:"mid"`
			Name   string `json:"name"`
			Singer string `json:"singer"`
			Album  string `json:"album"`
		} `json:"songs"`
	}
	if err := c.getJSON(ctx, "/api/search", q, &out); err != nil {
		return nil, err
	}
	results := make([]SearchResult, 0, len(out.Songs))
	for _, s := range out.Songs {
		if s.Mid == "" {
			continue
		}
		results = append(results, SearchResult{
			ID:      s.Mid,
			Name:    s.Name,
			Artists: splitSingers(s.Singer),
			Album:   s.Album,
		})
	}
	return results, nil
}

// TrackURL resolves a fresh OGG_192 CDN link for a song mid. It returns an
// error when the account lacks rights for the track (the service returns a null
// url) or the account credential has expired.
func (c *Client) TrackURL(ctx context.Context, mid string) (string, error) {
	if strings.TrimSpace(mid) == "" {
		return "", fmt.Errorf("qqmusic: mid is required")
	}
	q := url.Values{}
	q.Set("mid", mid)

	var out struct {
		OggURL  *string `json:"ogg_url"`
		Expired bool    `json:"expired"`
	}
	if err := c.getJSON(ctx, "/api/url", q, &out); err != nil {
		return "", err
	}
	if out.OggURL == nil || strings.TrimSpace(*out.OggURL) == "" {
		if out.Expired {
			return "", fmt.Errorf("qqmusic: account credential expired, re-scan login required")
		}
		return "", fmt.Errorf("qqmusic: no playable url for mid %s (account lacks rights?)", mid)
	}
	return *out.OggURL, nil
}

// getJSON issues an authenticated GET and decodes JSON into out. On a 401
// (expired/absent sid cookie) it logs in once and retries the request a single
// time.
func (c *Client) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	endpoint := c.baseURL + path + "?" + q.Encode()

	do := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("qqmusic: build request: %w", err)
		}
		return c.httpClient.Do(req)
	}

	resp, err := do()
	if err != nil {
		return fmt.Errorf("qqmusic: request %s: %w", path, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		// sid missing or expired: re-login, then retry once.
		if err := c.Login(ctx); err != nil {
			return err
		}
		resp, err = do()
		if err != nil {
			return fmt.Errorf("qqmusic: request %s (after re-login): %w", path, err)
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("qqmusic: %s returned HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("qqmusic: decode %s response: %w", path, err)
	}
	return nil
}

// splitSingers turns the service's 、-joined singer string into a slice. An
// empty or placeholder value yields a single-element slice so the UI always has
// something to show.
func splitSingers(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "、")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
