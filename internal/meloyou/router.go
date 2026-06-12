// Package meloyou routes music box search and URL resolution to a backend
// chosen by source id. netease and bilibili go through the GD Studio API
// (internal/gdmusic); kuwo and tencent (QQ) can't use that API — kuwo resolves
// to an empty url there and QQ search is unsupported — so they have their own
// backends here, talking to the platforms' own search endpoints plus a set of
// free URL-resolve endpoints.
//
// The free resolve endpoints are third-party and flaky. kuwo tries several in
// order and takes the first that answers; QQ has a single one, so a QQ resolve
// failure simply marks the track failed without affecting other sources.
package meloyou

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zhuangkaiyi/gang-chat/server/internal/gdmusic"
)

// Track is one search result. We deliberately omit album art: kuwo/QQ don't
// resolve a usable pic url through these endpoints and the client renders a
// placeholder regardless.
type Track struct {
	ID      string
	Name    string
	Artists []string
	Source  string
}

// Resolved is a playable URL for a track.
type Resolved struct {
	URL string
}

// Router dispatches by source id. Build one with New.
type Router struct {
	gd         *gdmusic.Client
	httpClient *http.Client

	// Endpoint templates, overridable in tests. kuwo resolve templates all
	// share the {"code":..,"data":{"url":..}} response shape and carry "{id}"
	// where the track id goes; they're tried in order until one yields a url.
	kuwoSearchURL    string
	kuwoResolveTmpls []string
	qqSearchURL      string
	qqResolveTmpl    string // carries "{mid}"
}

// Option configures a Router.
type Option func(*Router)

// WithHTTPClient overrides the HTTP client used for kuwo/QQ (useful in tests).
func WithHTTPClient(hc *http.Client) Option {
	return func(r *Router) {
		if hc != nil {
			r.httpClient = hc
		}
	}
}

// WithKuwoEndpoints overrides kuwo's search URL and ordered resolve templates.
func WithKuwoEndpoints(searchURL string, resolveTmpls []string) Option {
	return func(r *Router) {
		if searchURL != "" {
			r.kuwoSearchURL = searchURL
		}
		if len(resolveTmpls) > 0 {
			r.kuwoResolveTmpls = resolveTmpls
		}
	}
}

// WithQQEndpoints overrides QQ's search URL and resolve template.
func WithQQEndpoints(searchURL, resolveTmpl string) Option {
	return func(r *Router) {
		if searchURL != "" {
			r.qqSearchURL = searchURL
		}
		if resolveTmpl != "" {
			r.qqResolveTmpl = resolveTmpl
		}
	}
}

// cyapi.top ships with this public apikey baked into the plugin the endpoints
// came from; it's not a secret we issued.
const cyapiKey = "2baf39266d8ef0580aba937245d5bb569fe376f230ff508f1faa0922dc320fe4"

// New builds a Router. gd is the configured GD Studio client used for the
// netease/bilibili passthrough; it must be non-nil.
func New(gd *gdmusic.Client, opts ...Option) *Router {
	r := &Router{
		gd:            gd,
		httpClient:    &http.Client{Timeout: 15 * time.Second},
		kuwoSearchURL: "https://search.kuwo.cn/r.s",
		kuwoResolveTmpls: []string{
			"https://musicapi.haitangw.net/music/kw.php?type=json&id={id}&level=320kmp3",
			"http://music.nxinxz.com/kw.php?id={id}&level=320kmp3&type=json",
			"https://kw-api.cenguigui.cn/?id={id}&type=song&level=320k&format=json",
		},
		qqSearchURL:   "https://u.y.qq.com/cgi-bin/musicu.fcg",
		qqResolveTmpl: "https://cyapi.top/API/qq_music.php?apikey=" + cyapiKey + "&msg=&num=20&type=json&n=1&mid={mid}",
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Search queries the backend for the given source. An empty source falls back
// to the GD client's default (netease).
func (r *Router) Search(ctx context.Context, source, keyword string, count, page int) ([]Track, error) {
	if strings.TrimSpace(keyword) == "" {
		return nil, fmt.Errorf("meloyou: keyword is required")
	}
	switch source {
	case "kuwo":
		return r.kuwoSearch(ctx, keyword, count)
	case "tencent":
		return r.qqSearch(ctx, keyword, count)
	default:
		// netease, bilibili, or empty -> GD passthrough.
		results, err := r.gd.Search(ctx, source, keyword, count, page)
		if err != nil {
			return nil, err
		}
		tracks := make([]Track, len(results))
		for i, res := range results {
			tracks[i] = Track{ID: res.ID, Name: res.Name, Artists: res.Artists, Source: res.Source}
		}
		return tracks, nil
	}
}

// TrackURL resolves a playable URL. bitrate is honored only by the GD
// passthrough; kuwo/QQ request a fixed quality from their free endpoints.
func (r *Router) TrackURL(ctx context.Context, source, trackID, bitrate string) (*Resolved, error) {
	if strings.TrimSpace(trackID) == "" {
		return nil, fmt.Errorf("meloyou: track id is required")
	}
	switch source {
	case "kuwo":
		return r.kuwoTrackURL(ctx, trackID)
	case "tencent":
		return r.qqTrackURL(ctx, trackID)
	default:
		out, err := r.gd.TrackURL(ctx, source, trackID, bitrate)
		if err != nil {
			return nil, err
		}
		return &Resolved{URL: out.URL}, nil
	}
}
