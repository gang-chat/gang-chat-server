package meloyou

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// qqSearch queries QQ Music's desktop search service. It returns each track's
// mid (a string id like "00476zUy2oLuhh"), which qqTrackURL resolves.
func (r *Router) qqSearch(ctx context.Context, keyword string, count int) ([]Track, error) {
	if count <= 0 {
		count = 20
	}
	reqBody := map[string]any{
		"req_0": map[string]any{
			"module": "music.search.SearchCgiService",
			"method": "DoSearchForQQMusicDesktop",
			"param": map[string]any{
				"query":        keyword,
				"num_per_page": count,
				"page_num":     1,
				"search_type":  0,
			},
		},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	var raw struct {
		Req0 struct {
			Code int `json:"code"`
			Data struct {
				Body struct {
					Song struct {
						List []struct {
							Mid    string `json:"mid"`
							Name   string `json:"name"`
							Singer []struct {
								Name string `json:"name"`
							} `json:"singer"`
						} `json:"list"`
					} `json:"song"`
				} `json:"body"`
			} `json:"data"`
		} `json:"req_0"`
	}
	if err := r.postJSON(ctx, r.qqSearchURL, bytes.NewReader(payload), &raw); err != nil {
		return nil, fmt.Errorf("qq search: %w", err)
	}
	if raw.Req0.Code != 0 {
		return nil, fmt.Errorf("qq search: upstream code %d", raw.Req0.Code)
	}
	list := raw.Req0.Data.Body.Song.List
	tracks := make([]Track, 0, len(list))
	for _, s := range list {
		if s.Mid == "" {
			continue
		}
		artists := make([]string, 0, len(s.Singer))
		for _, sg := range s.Singer {
			if n := strings.TrimSpace(sg.Name); n != "" {
				artists = append(artists, n)
			}
		}
		tracks = append(tracks, Track{
			ID:      s.Mid,
			Name:    s.Name,
			Artists: artists,
			Source:  "tencent",
		})
	}
	return tracks, nil
}

// qqTrackURL resolves a QQ mid to a playable url via the single free endpoint
// (cyapi.top). Unlike kuwo there's no fallback, so a failure here just fails
// this one track.
func (r *Router) qqTrackURL(ctx context.Context, mid string) (*Resolved, error) {
	endpoint := strings.ReplaceAll(r.qqResolveTmpl, "{mid}", url.QueryEscape(mid))
	var out struct {
		URL string `json:"url"`
	}
	if err := r.getJSON(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, fmt.Errorf("qq resolve %s: %w", mid, err)
	}
	if u := strings.TrimSpace(out.URL); u != "" {
		return &Resolved{URL: u}, nil
	}
	return nil, fmt.Errorf("qq resolve %s: empty url", mid)
}

// postJSON performs a POST with a JSON body and decodes the response. QQ's
// search service requires a y.qq.com Referer or it rejects the request.
func (r *Router) postJSON(ctx context.Context, endpoint string, body *bytes.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://y.qq.com")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return decodeBody(resp, out)
}
