package chat

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/zhuangkaiyi/gang-chat/server/internal/musicbox"
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

	status, response = api.request(
		http.MethodPatch,
		"/me/music-box/playlists/"+playlistID,
		owner.Token,
		map[string]any{"name": "夜间精选"},
	)
	api.requireStatus(status, http.StatusOK, response)
	renamed := response["playlist"].(map[string]any)
	if renamed["name"] != "夜间精选" || renamed["revision"] != float64(2) {
		t.Fatalf("unexpected renamed playlist: %v", renamed)
	}

	status, response = api.request(
		http.MethodPatch,
		"/me/music-box/playlists/"+playlistID,
		other.Token,
		map[string]any{"name": "越权重命名"},
	)
	api.requireStatus(status, http.StatusNotFound, response)

	status, response = api.request(
		http.MethodPatch,
		"/me/music-box/playlists/"+playlistID,
		owner.Token,
		map[string]any{"name": " "},
	)
	api.requireStatus(status, http.StatusBadRequest, response)

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

func TestPersonalMusicPlaylistMergePreservesSelectionOrderAndDeduplicatesLinks(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("playlist_merge_owner")
	other := api.register("playlist_merge_other")
	ownerID := owner.User["id"].(string)

	createPlaylist := func(token, name string) string {
		status, response := api.request(
			http.MethodPost,
			"/me/music-box/playlists",
			token,
			map[string]any{"name": name},
		)
		api.requireStatus(status, http.StatusCreated, response)
		return response["playlist"].(map[string]any)["id"].(string)
	}
	addTrack := func(playlistID, trackID, title, source string) {
		status, response := api.request(
			http.MethodPost,
			"/me/music-box/playlists/"+playlistID+"/items",
			owner.Token,
			map[string]any{
				"track_id": trackID,
				"source":   source,
				"title":    title,
				"artists":  []string{"歌手"},
			},
		)
		api.requireStatus(status, http.StatusCreated, response)
	}

	firstID := createPlaylist(owner.Token, "第一来源")
	secondID := createPlaylist(owner.Token, "第二来源")
	foreignID := createPlaylist(other.Token, "外部来源")
	addTrack(firstID, "link-a", "A", "netease")
	addTrack(firstID, "link-b", "B", "netease")
	addTrack(firstID, "link-a", "A 重复", "netease")
	addTrack(secondID, "link-b", "B 重复", "netease")
	addTrack(secondID, "link-c", "C", "netease")
	// The same external id on another source is a different concrete link.
	addTrack(secondID, "link-b", "B 哔哩哔哩", "bilibili")
	for index := 0; index < musicbox.MaxUserPlaylists-2; index++ {
		if _, err := api.chat.Playlists.CreateUserPlaylist(
			context.Background(),
			ownerID,
			fmt.Sprintf("填满槽位 %02d", index+1),
		); err != nil {
			t.Fatalf("fill playlist slot %d: %v", index+1, err)
		}
	}

	status, response := api.request(
		http.MethodPost,
		"/me/music-box/playlists/merge",
		owner.Token,
		map[string]any{
			"name":         "合并结果",
			"playlist_ids": []string{secondID, firstID},
		},
	)
	api.requireStatus(status, http.StatusCreated, response)
	playlist := response["playlist"].(map[string]any)
	merge := response["merge"].(map[string]any)
	if playlist["name"] != "合并结果" || playlist["item_count"] != float64(4) {
		t.Fatalf("unexpected merged playlist: %v", playlist)
	}
	if merge["source_item_count"] != float64(6) ||
		merge["unique_item_count"] != float64(4) ||
		merge["duplicate_count"] != float64(2) ||
		merge["deleted_playlist_count"] != float64(2) ||
		merge["retained_playlist_count"] != float64(0) ||
		merge["truncated"] != false {
		t.Fatalf("unexpected merge stats: %v", merge)
	}

	mergedID := playlist["id"].(string)
	status, response = api.request(
		http.MethodGet,
		"/me/music-box/playlists/"+mergedID,
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	items := response["items"].([]any)
	// Repeated concrete links share one track row, so the latest metadata
	// snapshot is displayed while the first-link position is preserved.
	wantTitles := []string{"B 重复", "C", "B 哔哩哔哩", "A 重复"}
	if len(items) != len(wantTitles) {
		t.Fatalf("merged items = %v", items)
	}
	for index, want := range wantTitles {
		if got := items[index].(map[string]any)["title"]; got != want {
			t.Fatalf("merged title %d = %v, want %q; items=%v", index, got, want, items)
		}
	}

	status, response = api.request(
		http.MethodPost,
		"/me/music-box/playlists/merge",
		owner.Token,
		map[string]any{
			"name":         "不能越权合并",
			"playlist_ids": []string{firstID, foreignID},
		},
	)
	api.requireStatus(status, http.StatusNotFound, response)
}

func TestPersonalMusicPlaylistBatchAddCopiesWithoutMutatingSource(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("playlist_batch_add_owner")
	other := api.register("playlist_batch_add_other")

	createPlaylist := func(token, name string) string {
		status, response := api.request(
			http.MethodPost,
			"/me/music-box/playlists",
			token,
			map[string]any{"name": name},
		)
		api.requireStatus(status, http.StatusCreated, response)
		return response["playlist"].(map[string]any)["id"].(string)
	}
	addTrack := func(playlistID, trackID, title string) string {
		status, response := api.request(
			http.MethodPost,
			"/me/music-box/playlists/"+playlistID+"/items",
			owner.Token,
			map[string]any{
				"track_id": trackID,
				"source":   "netease",
				"title":    title,
				"artists":  []string{"歌手"},
			},
		)
		api.requireStatus(status, http.StatusCreated, response)
		return response["item"].(map[string]any)["id"].(string)
	}

	sourceID := createPlaylist(owner.Token, "批量添加来源")
	targetID := createPlaylist(owner.Token, "批量添加目标")
	foreignID := createPlaylist(other.Token, "其他人的目标")
	itemA := addTrack(sourceID, "batch-link-a", "A")
	itemB := addTrack(sourceID, "batch-link-b", "B")
	itemBDuplicate := addTrack(sourceID, "batch-link-b", "B 重复")
	addTrack(targetID, "batch-link-a", "A 已存在")

	status, response := api.request(
		http.MethodPost,
		"/me/music-box/playlists/"+targetID+"/items/batch-add",
		owner.Token,
		map[string]any{
			"source_playlist_id": sourceID,
			"item_ids":           []string{itemB, itemA, itemBDuplicate},
		},
	)
	api.requireStatus(status, http.StatusOK, response)
	batch := response["batch_add"].(map[string]any)
	if batch["selected_item_count"] != float64(3) ||
		batch["unique_item_count"] != float64(2) ||
		batch["duplicate_count"] != float64(1) ||
		batch["already_present_count"] != float64(1) ||
		batch["added_item_count"] != float64(1) ||
		batch["omitted_count"] != float64(0) {
		t.Fatalf("unexpected batch add stats: %v", batch)
	}

	status, response = api.request(
		http.MethodGet,
		"/me/music-box/playlists/"+sourceID,
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	if sourceItems := response["items"].([]any); len(sourceItems) != 3 {
		t.Fatalf("source items changed after batch add: %v", sourceItems)
	}
	status, response = api.request(
		http.MethodGet,
		"/me/music-box/playlists/"+targetID,
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	targetItems := response["items"].([]any)
	if len(targetItems) != 2 ||
		targetItems[0].(map[string]any)["track_id"] != "batch-link-a" ||
		targetItems[1].(map[string]any)["track_id"] != "batch-link-b" {
		t.Fatalf("unexpected target items/order: %v", targetItems)
	}

	status, response = api.request(
		http.MethodPost,
		"/me/music-box/playlists/"+foreignID+"/items/batch-add",
		owner.Token,
		map[string]any{
			"source_playlist_id": sourceID,
			"item_ids":           []string{itemA},
		},
	)
	api.requireStatus(status, http.StatusNotFound, response)
}

func TestRoomMusicPlaylistPermissionsAndIsolation(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("room_playlist_owner")
	member := api.register("room_playlist_member")
	outsider := api.register("room_playlist_outsider")
	room := api.createRoom(owner.Token, map[string]any{
		"name":        "Room Playlist A",
		"join_policy": "open",
	})
	roomID := room["id"].(string)
	otherRoom := api.createRoom(owner.Token, map[string]any{
		"name":        "Room Playlist B",
		"join_policy": "open",
	})
	otherRoomID := otherRoom["id"].(string)
	status, response := api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/join",
		member.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/music-box/playlists",
		member.Token,
		map[string]any{"name": "member cannot create"},
	)
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/music-box/playlists",
		owner.Token,
		map[string]any{"name": "房间精选"},
	)
	api.requireStatus(status, http.StatusCreated, response)
	playlist := response["playlist"].(map[string]any)
	playlistID := playlist["id"].(string)

	status, response = api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/music-box/playlists",
		owner.Token,
		map[string]any{"name": "房间精选"},
	)
	api.requireStatus(status, http.StatusConflict, response)
	if code := responseErrorCode(response); code != "playlist_name_conflict" {
		t.Fatalf("duplicate room playlist name code = %q, response=%v", code, response)
	}

	status, response = api.request(
		http.MethodGet,
		"/rooms/"+roomID+"/music-box/playlists",
		member.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	playlists := response["playlists"].([]any)
	if len(playlists) != 1 || playlists[0].(map[string]any)["id"] != playlistID {
		t.Fatalf("member should see the room playlist: %v", response)
	}

	status, response = api.request(
		http.MethodGet,
		"/rooms/"+roomID+"/music-box/playlists",
		outsider.Token,
		nil,
	)
	api.requireStatus(status, http.StatusNotFound, response)

	track := map[string]any{
		"track_id": "room_track_1",
		"source":   "netease",
		"title":    "房间歌曲",
		"artists":  []string{"歌手"},
	}
	status, response = api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/music-box/playlists/"+playlistID+"/items",
		owner.Token,
		track,
	)
	api.requireStatus(status, http.StatusCreated, response)
	itemID := response["item"].(map[string]any)["id"].(string)

	status, response = api.request(
		http.MethodGet,
		"/rooms/"+roomID+"/music-box/playlists/"+playlistID,
		member.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	items := response["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != itemID {
		t.Fatalf("member should see room playlist items: %v", response)
	}

	status, response = api.request(
		http.MethodDelete,
		"/rooms/"+roomID+"/music-box/playlists/"+playlistID+"/items/"+itemID,
		member.Token,
		nil,
	)
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(
		http.MethodGet,
		"/rooms/"+otherRoomID+"/music-box/playlists/"+playlistID,
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusNotFound, response)

	status, response = api.request(
		http.MethodPatch,
		"/rooms/"+otherRoomID+"/music-box/playlists/"+playlistID,
		owner.Token,
		map[string]any{"name": "cross-room rename"},
	)
	api.requireStatus(status, http.StatusNotFound, response)

	status, response = api.request(
		http.MethodDelete,
		"/rooms/"+roomID+"/music-box/playlists/"+playlistID,
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
}

func TestRoomMusicPlaylistCanAtomicallyImportOwnersPersonalPlaylist(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("room_playlist_import_owner")
	other := api.register("room_playlist_import_other")
	room := api.createRoom(owner.Token, map[string]any{
		"name":        "Room Playlist Import",
		"join_policy": "open",
	})
	roomID := room["id"].(string)

	status, response := api.request(
		http.MethodPost,
		"/me/music-box/playlists",
		owner.Token,
		map[string]any{"name": "我的导入源"},
	)
	api.requireStatus(status, http.StatusCreated, response)
	sourceID := response["playlist"].(map[string]any)["id"].(string)
	for _, track := range []map[string]any{
		{
			"track_id": "import_track_1", "source": "netease",
			"title": "第一首", "artists": []string{"歌手甲"},
		},
		{
			"track_id": "import_track_2", "source": "bilibili",
			"title": "第二首", "artists": []string{"歌手乙"},
		},
	} {
		status, response = api.request(
			http.MethodPost,
			"/me/music-box/playlists/"+sourceID+"/items",
			owner.Token,
			track,
		)
		api.requireStatus(status, http.StatusCreated, response)
	}

	status, response = api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/music-box/playlists",
		owner.Token,
		map[string]any{
			"name":               "房间导入副本",
			"import_playlist_id": sourceID,
		},
	)
	api.requireStatus(status, http.StatusCreated, response)
	created := response["playlist"].(map[string]any)
	if created["item_count"] != float64(2) {
		t.Fatalf("imported item_count = %v, want 2", created["item_count"])
	}
	targetID := created["id"].(string)

	status, response = api.request(
		http.MethodGet,
		"/rooms/"+roomID+"/music-box/playlists/"+targetID,
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	items := response["items"].([]any)
	if len(items) != 2 ||
		items[0].(map[string]any)["title"] != "第一首" ||
		items[1].(map[string]any)["title"] != "第二首" {
		t.Fatalf("imported order/content = %v", items)
	}

	status, response = api.request(
		http.MethodPost,
		"/me/music-box/playlists",
		other.Token,
		map[string]any{"name": "他人的歌单"},
	)
	api.requireStatus(status, http.StatusCreated, response)
	foreignID := response["playlist"].(map[string]any)["id"].(string)
	status, response = api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/music-box/playlists",
		owner.Token,
		map[string]any{
			"name":               "不能导入",
			"import_playlist_id": foreignID,
		},
	)
	api.requireStatus(status, http.StatusNotFound, response)
}

func TestRoomMusicPlaylistMergeRequiresAdminAndUsesRoomScope(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("room_playlist_merge_owner")
	member := api.register("room_playlist_merge_member")
	room := api.createRoom(owner.Token, map[string]any{
		"name":        "Room Playlist Merge",
		"join_policy": "open",
	})
	roomID := room["id"].(string)
	status, response := api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/join",
		member.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)

	playlistIDs := make([]string, 0, 2)
	for _, name := range []string{"房间一", "房间二"} {
		status, response = api.request(
			http.MethodPost,
			"/rooms/"+roomID+"/music-box/playlists",
			owner.Token,
			map[string]any{"name": name},
		)
		api.requireStatus(status, http.StatusCreated, response)
		playlistIDs = append(
			playlistIDs,
			response["playlist"].(map[string]any)["id"].(string),
		)
	}
	for index, playlistID := range playlistIDs {
		status, response = api.request(
			http.MethodPost,
			"/rooms/"+roomID+"/music-box/playlists/"+playlistID+"/items",
			owner.Token,
			map[string]any{
				"track_id": fmt.Sprintf("room-merge-link-%d", index),
				"source":   "netease",
				"title":    fmt.Sprintf("房间歌曲%d", index+1),
				"artists":  []string{"歌手"},
			},
		)
		api.requireStatus(status, http.StatusCreated, response)
	}

	requestBody := map[string]any{
		"name":         "房间合并结果",
		"playlist_ids": playlistIDs,
	}
	status, response = api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/music-box/playlists/merge",
		member.Token,
		requestBody,
	)
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/music-box/playlists/merge",
		owner.Token,
		requestBody,
	)
	api.requireStatus(status, http.StatusCreated, response)
	playlist := response["playlist"].(map[string]any)
	if playlist["name"] != "房间合并结果" || playlist["item_count"] != float64(2) {
		t.Fatalf("unexpected room merge: %v", response)
	}
}

func TestRoomMusicPlaylistBatchAddRequiresAdmin(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("room_playlist_batch_owner")
	member := api.register("room_playlist_batch_member")
	room := api.createRoom(owner.Token, map[string]any{
		"name":        "Room Playlist Batch Add",
		"join_policy": "open",
	})
	roomID := room["id"].(string)
	status, response := api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/join",
		member.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)

	playlistIDs := make([]string, 0, 2)
	for _, name := range []string{"房间来源", "房间目标"} {
		status, response = api.request(
			http.MethodPost,
			"/rooms/"+roomID+"/music-box/playlists",
			owner.Token,
			map[string]any{"name": name},
		)
		api.requireStatus(status, http.StatusCreated, response)
		playlistIDs = append(
			playlistIDs,
			response["playlist"].(map[string]any)["id"].(string),
		)
	}
	status, response = api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/music-box/playlists/"+playlistIDs[0]+"/items",
		owner.Token,
		map[string]any{
			"track_id": "room-batch-link",
			"source":   "netease",
			"title":    "房间批量歌曲",
			"artists":  []string{"歌手"},
		},
	)
	api.requireStatus(status, http.StatusCreated, response)
	itemID := response["item"].(map[string]any)["id"].(string)
	requestBody := map[string]any{
		"source_playlist_id": playlistIDs[0],
		"item_ids":           []string{itemID},
	}

	status, response = api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/music-box/playlists/"+playlistIDs[1]+"/items/batch-add",
		member.Token,
		requestBody,
	)
	api.requireStatus(status, http.StatusForbidden, response)
	status, response = api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/music-box/playlists/"+playlistIDs[1]+"/items/batch-add",
		owner.Token,
		requestBody,
	)
	api.requireStatus(status, http.StatusOK, response)
	if response["batch_add"].(map[string]any)["added_item_count"] != float64(1) {
		t.Fatalf("unexpected room batch add response: %v", response)
	}
}

func TestRoomMusicPlaylistCloneUsesRemarkAndEnforcesCapacity(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("room_playlist_clone_owner")
	outsider := api.register("room_playlist_clone_outsider")
	ownerID := owner.User["id"].(string)
	room := api.createRoom(owner.Token, map[string]any{
		"name":        "原房间名",
		"join_policy": "open",
	})
	roomID := room["id"].(string)
	if _, err := api.db.Exec(
		`UPDATE room_memberships SET remark_name = ? WHERE room_id = ? AND user_id = ?`,
		"房间备注名",
		roomID,
		ownerID,
	); err != nil {
		t.Fatalf("set room remark: %v", err)
	}

	playlistIDs := make([]string, 0, 3)
	for _, name := range []string{"第一", "第二", "第三"} {
		status, response := api.request(
			http.MethodPost,
			"/rooms/"+roomID+"/music-box/playlists",
			owner.Token,
			map[string]any{"name": name},
		)
		api.requireStatus(status, http.StatusCreated, response)
		playlistIDs = append(
			playlistIDs,
			response["playlist"].(map[string]any)["id"].(string),
		)
	}
	status, response := api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/music-box/playlists/"+playlistIDs[2]+"/items",
		owner.Token,
		map[string]any{
			"track_id": "clone_track_3",
			"source":   "netease",
			"title":    "第三首歌",
			"artists":  []string{"歌手丙"},
		},
	)
	api.requireStatus(status, http.StatusCreated, response)

	for index := 0; index < musicbox.MaxUserPlaylists-1; index++ {
		if _, err := api.chat.Playlists.CreateUserPlaylist(
			context.Background(),
			ownerID,
			fmt.Sprintf("已有歌单 %02d", index+1),
		); err != nil {
			t.Fatalf("fill personal playlist slot %d: %v", index+1, err)
		}
	}

	status, response = api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/music-box/playlists/"+playlistIDs[2]+"/clone-to-me",
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusCreated, response)
	clonedPlaylist := response["playlist"].(map[string]any)
	if clonedPlaylist["name"] != "房间备注名 · 第三" || clonedPlaylist["item_count"] != float64(1) {
		t.Fatalf("unexpected cloned playlist: %v", clonedPlaylist)
	}
	clonedID := clonedPlaylist["id"].(string)

	status, response = api.request(
		http.MethodGet,
		"/me/music-box/playlists/"+clonedID,
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	items := response["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["title"] != "第三首歌" {
		t.Fatalf("cloned items = %v", items)
	}

	status, response = api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/music-box/playlists/"+playlistIDs[0]+"/clone-to-me",
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusConflict, response)
	if code := responseErrorCode(response); code != "playlist_limit_reached" {
		t.Fatalf("full personal library clone code = %q, response=%v", code, response)
	}

	status, response = api.request(
		http.MethodPost,
		"/rooms/"+roomID+"/music-box/playlists/"+playlistIDs[0]+"/clone-to-me",
		outsider.Token,
		nil,
	)
	api.requireStatus(status, http.StatusNotFound, response)
}

func TestMusicBoxRequesterPayloadsIncludesExplicitPlaylistOwner(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("playlist_payload_owner")
	ownerID, _ := owner.User["id"].(string)
	if ownerID == "" {
		t.Fatalf("registered owner missing id: %v", owner.User)
	}
	if _, err := api.db.Exec(
		`UPDATE users
		 SET display_name = ?, avatar_url = ?, default_avatar_key = ?
		 WHERE id = ?`,
		"歌单用户", "/playlist-owner.png", "green-2", ownerID,
	); err != nil {
		t.Fatalf("update playlist owner: %v", err)
	}

	payloads := api.chat.musicBoxRequesterPayloads("", nil, ownerID)
	payload := payloads[ownerID]
	if payload == nil {
		t.Fatalf("explicit playlist owner missing from payload: %v", payloads)
	}
	if payload["display_name"] != "歌单用户" ||
		payload["avatar_label"] != "歌单用户" ||
		payload["avatar_url"] != "/playlist-owner.png" ||
		payload["default_avatar_key"] != "green-2" {
		t.Fatalf("unexpected playlist owner payload: %v", payload)
	}
}

func responseErrorCode(response map[string]any) string {
	errorPayload, _ := response["error"].(map[string]any)
	code, _ := errorPayload["code"].(string)
	return code
}
