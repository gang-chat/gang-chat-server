// Package gdmusic is a thin client for the GD Studio music platform API
// (https://music.gdstudio.xyz). It covers the four endpoints the room music
// box needs: search, track URL resolution, album art, and lyrics.
//
// The platform is study-only and rate limited (≤50 requests / 5 minutes per
// the published docs). We respect that with a small token-bucket limiter and
// attribute the source via User-Agent, as the docs request.
package gdmusic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://music-api.gdstudio.xyz/api.php"
	// Attribution requested by the API docs: 出处 "GD音乐台(music.gdstudio.xyz)".
	userAgent = "gang-chat-music-box (+https://music.gdstudio.xyz GD音乐台)"
)

// Client talks to the GD Studio API. The zero value is not usable; call New.
type Client struct {
	httpClient  *http.Client
	baseURL     string
	limiter     *rateLimiter
	defaultSrc  string
	defaultBR   string
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the default HTTP client (useful in tests).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// WithBaseURL overrides the API base URL (useful in tests).
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.baseURL = u
		}
	}
}

// WithDefaultSource sets the music source used when a request omits one.
func WithDefaultSource(src string) Option {
	return func(c *Client) {
		if src != "" {
			c.defaultSrc = src
		}
	}
}

// WithDefaultBitrate sets the audio quality requested from TrackURL when a
// call passes an empty bitrate. The docs accept 128/192/320/740/999.
func WithDefaultBitrate(br string) Option {
	return func(c *Client) {
		if br != "" {
			c.defaultBR = br
		}
	}
}

// New builds a Client. Defaults: netease source, 192 kbps, 15s timeout, and a
// limiter capped at 50 requests / 5 minutes to stay within the published quota.
func New(opts ...Option) *Client {
	c := &Client{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		baseURL:    defaultBaseURL,
		limiter:    newRateLimiter(50, 5*time.Minute),
		defaultSrc: "netease",
		defaultBR:  "192",
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// SearchResult is one track from a search response.
type SearchResult struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Artists  []string `json:"artist"`
	Album    string   `json:"album"`
	PicID    string   `json:"pic_id"`
	LyricID  string   `json:"lyric_id"`
	Source   string   `json:"source"`
}

// TrackURL is the resolved playable URL for a track.
type TrackURL struct {
	URL     string `json:"url"`
	Bitrate int    `json:"br"`
	SizeKB  int64  `json:"size"`
}

// Lyric holds LRC lyrics; TranslatedLyric may be empty.
type Lyric struct {
	Lyric           string `json:"lyric"`
	TranslatedLyric string `json:"tlyric"`
}

// Search queries the platform. source may be empty to use the client default;
// count and page default to 20 and 1 when non-positive.
func (c *Client) Search(ctx context.Context, source, keyword string, count, page int) ([]SearchResult, error) {
	if strings.TrimSpace(keyword) == "" {
		return nil, fmt.Errorf("gdmusic: keyword is required")
	}
	if count <= 0 {
		count = 20
	}
	if page <= 0 {
		page = 1
	}
	q := url.Values{}
	q.Set("types", "search")
	q.Set("source", c.source(source))
	q.Set("name", keyword)
	q.Set("count", strconv.Itoa(count))
	q.Set("pages", strconv.Itoa(page))

	var results []SearchResult
	if err := c.getJSON(ctx, q, &results); err != nil {
		return nil, err
	}
	// The API omits source on each row for some backends; backfill it so the
	// caller always knows which source a track id belongs to.
	src := c.source(source)
	for i := range results {
		if results[i].Source == "" {
			results[i].Source = src
		}
	}
	return results, nil
}

// TrackURL resolves a playable URL for a track id. bitrate may be empty to use
// the client default.
func (c *Client) TrackURL(ctx context.Context, source, trackID, bitrate string) (*TrackURL, error) {
	if strings.TrimSpace(trackID) == "" {
		return nil, fmt.Errorf("gdmusic: track id is required")
	}
	q := url.Values{}
	q.Set("types", "url")
	q.Set("source", c.source(source))
	q.Set("id", trackID)
	if bitrate == "" {
		bitrate = c.defaultBR
	}
	q.Set("br", bitrate)

	var out TrackURL
	if err := c.getJSON(ctx, q, &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.URL) == "" {
		return nil, fmt.Errorf("gdmusic: no playable url for track %s", trackID)
	}
	return &out, nil
}

// AlbumArt resolves an album art URL for a pic id. size may be 300 or 500;
// empty defaults to 300.
func (c *Client) AlbumArt(ctx context.Context, source, picID, size string) (string, error) {
	if strings.TrimSpace(picID) == "" {
		return "", fmt.Errorf("gdmusic: pic id is required")
	}
	q := url.Values{}
	q.Set("types", "pic")
	q.Set("source", c.source(source))
	q.Set("id", picID)
	if size != "" {
		q.Set("size", size)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := c.getJSON(ctx, q, &out); err != nil {
		return "", err
	}
	return out.URL, nil
}

// Lyric fetches LRC lyrics for a lyric id.
func (c *Client) Lyric(ctx context.Context, source, lyricID string) (*Lyric, error) {
	if strings.TrimSpace(lyricID) == "" {
		return nil, fmt.Errorf("gdmusic: lyric id is required")
	}
	q := url.Values{}
	q.Set("types", "lyric")
	q.Set("source", c.source(source))
	q.Set("id", lyricID)

	var out Lyric
	if err := c.getJSON(ctx, q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) source(s string) string {
	if strings.TrimSpace(s) == "" {
		return c.defaultSrc
	}
	return s
}

func (c *Client) getJSON(ctx context.Context, q url.Values, out any) error {
	if err := c.limiter.wait(ctx); err != nil {
		return err
	}
	endpoint := c.baseURL + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Cap the body to guard against a misbehaving upstream.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gdmusic: upstream status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("gdmusic: decode response: %w (body: %s)", err, truncate(string(body), 200))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
