package chat

import (
	"net/http"
	"testing"
)

func TestPersonalMusicPlaylistCRUDAndOwnership(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("playlist_owner")
	other := api.register("playlist_other")

	status, response := api.request(
		http.MethodPost,
		"/me/music-box/playlists",
		owner.Token,
		map[string]any{"name": "夜晚"},
	)
	api.requireStatus(status, http.StatusCreated, response)
	playlist, ok := response["playlist"].(map[string]any)
	if !ok {
		t.Fatalf("create response missing playlist: %v", response)
	}
	playlistID, _ := playlist["id"].(string)
	if playlistID == "" || playlist["name"] != "夜晚" {
		t.Fatalf("unexpected playlist: %v", playlist)
	}

	status, response = api.request(
		http.MethodPost,
		"/me/music-box/playlists",
		owner.Token,
		map[string]any{"name": "夜晚"},
	)
	api.requireStatus(status, http.StatusConflict, response)
	if code := responseErrorCode(response); code != "playlist_name_conflict" {
		t.Fatalf("duplicate name code = %q, response=%v", code, response)
	}

	for _, track := range []map[string]any{
		{
			"track_id": "track_1",
			"source":   "netease",
			"title":    "晴天",
			"artists":  []string{"周杰伦"},
		},
		{
			// Duplicate tracks are intentionally allowed in saved playlists.
			"track_id": "track_1",
			"source":   "netease",
			"title":    "晴天",
			"artists":  []string{"周杰伦"},
		},
	} {
		status, response = api.request(
			http.MethodPost,
			"/me/music-box/playlists/"+playlistID+"/items",
			owner.Token,
			track,
		)
		api.requireStatus(status, http.StatusCreated, response)
	}

	status, response = api.request(
		http.MethodGet,
		"/me/music-box/playlists?page=1&page_size=50",
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	playlists, ok := response["playlists"].([]any)
	if !ok || len(playlists) != 1 {
		t.Fatalf("unexpected playlist list: %v", response)
	}
	listed := playlists[0].(map[string]any)
	if listed["item_count"] != float64(2) {
		t.Fatalf("item_count = %v, want 2", listed["item_count"])
	}

	status, response = api.request(
		http.MethodGet,
		"/me/music-box/playlists/"+playlistID+"?page=1&page_size=1",
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	items := response["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("first page items = %v", items)
	}
	pagination := response["pagination"].(map[string]any)
	if pagination["total"] != float64(2) || pagination["has_more"] != true {
		t.Fatalf("unexpected pagination: %v", pagination)
	}
	firstItemID := items[0].(map[string]any)["id"].(string)

	status, response = api.request(
		http.MethodGet,
		"/me/music-box/playlists/"+playlistID+"?page=2&page_size=1",
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	secondItemID := response["items"].([]any)[0].(map[string]any)["id"].(string)

	status, response = api.request(
		http.MethodPatch,
		"/me/music-box/playlists/"+playlistID+"/items/order",
		owner.Token,
		map[string]any{"item_id": secondItemID, "direction": "up"},
	)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(
		http.MethodGet,
		"/me/music-box/playlists/"+playlistID+"?page=1&page_size=50",
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	reordered := response["items"].([]any)
	if reordered[0].(map[string]any)["id"] != secondItemID {
		t.Fatalf("items were not reordered: %v", reordered)
	}

	status, response = api.request(
		http.MethodDelete,
		"/me/music-box/playlists/"+playlistID+"/items",
		owner.Token,
		map[string]any{"item_ids": []string{firstItemID, secondItemID}},
	)
	api.requireStatus(status, http.StatusOK, response)
	if response["deleted"] != float64(2) {
		t.Fatalf("deleted = %v, want 2", response["deleted"])
	}

	status, response = api.request(
		http.MethodGet,
		"/me/music-box/playlists/"+playlistID,
		other.Token,
		nil,
	)
	api.requireStatus(status, http.StatusNotFound, response)

	status, response = api.request(
		http.MethodDelete,
		"/me/music-box/playlists/"+playlistID,
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
}

func TestPersonalMusicPlaylistBatchPinPersistsOrder(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("playlist_pin_owner")
	other := api.register("playlist_pin_other")

	playlistIDs := make([]string, 0, 3)
	for _, name := range []string{"第一", "第二", "第三"} {
		status, response := api.request(
			http.MethodPost,
			"/me/music-box/playlists",
			owner.Token,
			map[string]any{"name": name},
		)
		api.requireStatus(status, http.StatusCreated, response)
		playlist := response["playlist"].(map[string]any)
		playlistIDs = append(playlistIDs, playlist["id"].(string))
	}

	status, response := api.request(
		http.MethodPost,
		"/me/music-box/playlists",
		other.Token,
		map[string]any{"name": "其他人的歌单"},
	)
	api.requireStatus(status, http.StatusCreated, response)
	otherPlaylistID := response["playlist"].(map[string]any)["id"].(string)

	status, response = api.request(
		http.MethodPatch,
		"/me/music-box/playlists/order",
		owner.Token,
		map[string]any{
			"playlist_ids": []string{playlistIDs[2], playlistIDs[1]},
		},
	)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(
		http.MethodGet,
		"/me/music-box/playlists?page=1&page_size=50",
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	playlists := response["playlists"].([]any)
	gotOrder := make([]string, 0, len(playlists))
	for _, value := range playlists {
		gotOrder = append(gotOrder, value.(map[string]any)["id"].(string))
	}
	wantOrder := []string{playlistIDs[2], playlistIDs[1], playlistIDs[0]}
	for index := range wantOrder {
		if gotOrder[index] != wantOrder[index] {
			t.Fatalf("playlist order = %v, want %v", gotOrder, wantOrder)
		}
	}

	status, response = api.request(
		http.MethodPatch,
		"/me/music-box/playlists/order",
		owner.Token,
		map[string]any{
			"playlist_id": playlistIDs[0],
			"direction":   "up",
		},
	)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(
		http.MethodGet,
		"/me/music-box/playlists?page=1&page_size=50",
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	playlists = response["playlists"].([]any)
	gotOrder = gotOrder[:0]
	for _, value := range playlists {
		gotOrder = append(gotOrder, value.(map[string]any)["id"].(string))
	}
	wantOrder = []string{playlistIDs[2], playlistIDs[0], playlistIDs[1]}
	for index := range wantOrder {
		if gotOrder[index] != wantOrder[index] {
			t.Fatalf("playlist order after move = %v, want %v", gotOrder, wantOrder)
		}
	}

	status, response = api.request(
		http.MethodPatch,
		"/me/music-box/playlists/order",
		owner.Token,
		map[string]any{
			"playlist_id": playlistIDs[2],
			"direction":   "up",
		},
	)
	api.requireStatus(status, http.StatusConflict, response)

	status, response = api.request(
		http.MethodPatch,
		"/me/music-box/playlists/order",
		owner.Token,
		map[string]any{
			"playlist_ids": []string{otherPlaylistID},
		},
	)
	api.requireStatus(status, http.StatusConflict, response)
	if code := responseErrorCode(response); code != "playlist_order_conflict" {
		t.Fatalf("foreign playlist pin code = %q, response=%v", code, response)
	}

	status, response = api.request(
		http.MethodPatch,
		"/me/music-box/playlists/order",
		owner.Token,
		map[string]any{
			"playlist_ids": []string{playlistIDs[0], playlistIDs[0]},
		},
	)
	api.requireStatus(status, http.StatusBadRequest, response)
}

func responseErrorCode(response map[string]any) string {
	errorPayload, _ := response["error"].(map[string]any)
	code, _ := errorPayload["code"].(string)
	return code
}
