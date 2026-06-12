package meloyou

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// kuwoSearch queries kuwo's official search endpoint. It returns the rid for
// each track, which kuwoTrackURL later resolves to a playable url.
func (r *Router) kuwoSearch(ctx context.Context, keyword string, count int) ([]Track, error) {
	if count <= 0 {
		count = 20
	}
	q := url.Values{}
	q.Set("client", "kt")
	q.Set("pn", "0")
	q.Set("rn", fmt.Sprintf("%d", count))
	q.Set("all", keyword)
	q.Set("vipver", "1")
	q.Set("ft", "music")
	q.Set("encoding", "utf8")
	q.Set("rformat", "json")
	q.Set("mobi", "1")
	endpoint := r.kuwoSearchURL + "?" + q.Encode()

	var raw struct {
		AbsList []struct {
			RID    string `json:"DC_TARGETID"`
			Name   string `json:"NAME"`
			Artist string `json:"ARTIST"`
		} `json:"abslist"`
	}
	if err := r.getJSON(ctx, http.MethodGet, endpoint, nil, &raw); err != nil {
		return nil, fmt.Errorf("kuwo search: %w", err)
	}
	tracks := make([]Track, 0, len(raw.AbsList))
	for _, it := range raw.AbsList {
		if it.RID == "" {
			continue
		}
		tracks = append(tracks, Track{
			ID:      it.RID,
			Name:    it.Name,
			Artists: splitArtists(it.Artist),
			Source:  "kuwo",
		})
	}
	return tracks, nil
}

// kuwoTrackURL tries each configured resolve endpoint in order and returns the
// first playable url. The free endpoints are flaky, so each gets a short
// timeout and any failure just advances to the next.
func (r *Router) kuwoTrackURL(ctx context.Context, rid string) (*Resolved, error) {
	var lastErr error
	for _, tmpl := range r.kuwoResolveTmpls {
		endpoint := strings.ReplaceAll(tmpl, "{id}", url.QueryEscape(rid))
		epCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		var out struct {
			Data struct {
				URL string `json:"url"`
			} `json:"data"`
		}
		err := r.getJSON(epCtx, http.MethodGet, endpoint, nil, &out)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		if u := strings.TrimSpace(out.Data.URL); u != "" {
			return &Resolved{URL: u}, nil
		}
		lastErr = fmt.Errorf("empty url")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no resolve endpoints configured")
	}
	return nil, fmt.Errorf("kuwo resolve %s: %w", rid, lastErr)
}

// splitArtists turns kuwo's separated artist string into a slice. kuwo uses an
// ampersand between names; trim and drop empties.
func splitArtists(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '&' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// getJSON performs an HTTP request with a browser-like User-Agent and decodes a
// JSON response into out. body, when non-nil, is sent as the request body.
func (r *Router) getJSON(ctx context.Context, method, endpoint string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return decodeBody(resp, out)
}

// decodeBody reads a capped response body and unmarshals it into out, erroring
// on non-200 status. Shared by the kuwo (GET) and QQ (POST) request helpers.
func decodeBody(resp *http.Response, out any) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream status %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w (body: %s)", err, truncate(string(data), 200))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
