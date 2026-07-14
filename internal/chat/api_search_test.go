package chat

import (
	"github.com/zhuangkaiyi/gang-chat/server/internal/idgen"
	"net/http"
	"strings"
	"testing"
)

func TestSearchAllReturnsCategoriesAndRespectsMembership(t *testing.T) {
	api := newAPIHarness(t)
	super := api.login("GANG", "64n9-Ch47")
	owner := api.register("search_all_owner")
	member := api.register("search_all_member")

	teamRoom := api.createRoom(owner.Token, map[string]any{
		"name":        "Alpha Team",
		"join_policy": "open",
	})
	teamRoomID := teamRoom["id"].(string)
	discoveryRoom := api.createRoom(owner.Token, map[string]any{
		"name":        "Alpha Discovery",
		"join_policy": "open",
	})
	privateRoom := api.createRoom(owner.Token, map[string]any{
		"name":       "Alpha Private",
		"visibility": "private",
	})
	descriptionOnlyRoom := api.createRoom(owner.Token, map[string]any{
		"name":        "Neutral Room",
		"description": "Alpha descriptiononlyneedle",
		"join_policy": "open",
	})
	descriptionOnlyRoomID := descriptionOnlyRoom["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+teamRoomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+descriptionOnlyRoomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	api.sendMessage(owner.Token, teamRoomID, "Alpha roadmap is ready")
	status, response = api.request(http.MethodPost, "/rooms/"+teamRoomID+"/messages", owner.Token, map[string]any{
		"client_message_id": "search_file_" + idgen.New("client"),
		"type":              "file",
		"body":              "Alpha blueprint.pdf",
		"attachments": []any{
			map[string]any{
				"type": "file",
				"name": "Alpha blueprint.pdf",
				"asset": map[string]any{
					"id":        "asset_alpha",
					"url":       "/assets/alpha.pdf",
					"mime_type": "application/pdf",
					"filename":  "Alpha blueprint.pdf",
				},
			},
		},
	})
	api.requireStatus(status, http.StatusCreated, response)
	status, response = api.request(http.MethodPost, "/rooms/"+teamRoomID+"/messages", owner.Token, map[string]any{
		"client_message_id": "search_image_" + idgen.New("client"),
		"type":              "file",
		"body":              "Alpha image caption should not match files",
		"attachments": []any{
			map[string]any{
				"type": "file",
				"name": "screenshot.png",
				"asset": map[string]any{
					"id":        "asset_alpha_image",
					"url":       "/assets/alpha-image.png",
					"mime_type": "image/png",
					"filename":  "screenshot.png",
				},
			},
		},
	})
	api.requireStatus(status, http.StatusCreated, response)
	status, response = api.request(http.MethodPost, "/rooms/"+teamRoomID+"/messages", owner.Token, map[string]any{
		"client_message_id": "search_audio_" + idgen.New("client"),
		"type":              "audio",
		"body":              "Alpha voice note",
		"attachments": []any{
			map[string]any{
				"type": "file",
				"name": "voice-note.m4a",
				"asset": map[string]any{
					"id":        "asset_voice",
					"url":       "/assets/voice-note.m4a",
					"mime_type": "audio/mp4",
					"filename":  "voice-note.m4a",
				},
			},
		},
	})
	api.requireStatus(status, http.StatusCreated, response)
	api.sendMessage(owner.Token, privateRoom["id"].(string), "Alpha private note")

	status, response = api.request(http.MethodGet, "/search?q=Alpha&limit=5", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	initialCursors, ok := response["next_cursors"].(map[string]any)
	if !ok {
		t.Fatalf("q+limit search response should include next_cursors: %v", response)
	}
	for _, category := range []string{"my_rooms", "public_rooms", "messages", "files"} {
		if _, ok := initialCursors[category]; !ok {
			t.Fatalf("q+limit next_cursors should include %s: %v", category, response)
		}
	}
	initialTotals, ok := response["total_counts"].(map[string]any)
	if !ok {
		t.Fatalf("q+limit search response should include total_counts: %v", response)
	}
	for _, category := range []string{"my_rooms", "public_rooms", "messages", "files"} {
		if initialTotals[category] != float64(1) {
			t.Fatalf("q+limit total_counts[%s] should be 1: %v", category, response)
		}
	}

	myRooms := response["my_rooms"].([]any)
	if len(myRooms) != 1 || myRooms[0].(map[string]any)["id"] != teamRoomID {
		t.Fatalf("search should return joined matching room only in my_rooms: %v", response)
	}

	publicRooms := response["public_rooms"].([]any)
	if len(publicRooms) != 1 || publicRooms[0].(map[string]any)["id"] != discoveryRoom["id"] {
		t.Fatalf("search should return unjoined public matching room only in public_rooms: %v", response)
	}

	messages := response["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("search should return text message hits and exclude voice messages: %v", response)
	}
	messageHit := messages[0].(map[string]any)
	if messageHit["room"].(map[string]any)["id"] != teamRoomID || !strings.Contains(messageHit["message"].(map[string]any)["body"].(string), "roadmap") {
		t.Fatalf("search message hit should include room context and message: %v", messageHit)
	}

	files := response["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("search should return file hits: %v", response)
	}
	fileHit := files[0].(map[string]any)
	if fileHit["room"].(map[string]any)["id"] != teamRoomID || fileHit["message"].(map[string]any)["type"] != "file" {
		t.Fatalf("search file hit should include room context and file message: %v", fileHit)
	}

	status, response = api.request(http.MethodGet, "/search?q=descriptiononlyneedle&limit=5", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["my_rooms"].([]any)); got != 0 {
		t.Fatalf("my room search should not match room description, got %d: %v", got, response)
	}

	status, response = api.request(http.MethodGet, "/search?q=descriptiononlyneedle&limit=5", super.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["my_rooms"].([]any)); got != 0 {
		t.Fatalf("superuser room search should not match room description, got %d: %v", got, response)
	}

	status, response = api.request(http.MethodGet, "/search?q=image&limit=5", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["files"].([]any)); got != 0 {
		t.Fatalf("file search should ignore attachment mime, url, id, and message body, got %d: %v", got, response)
	}

	status, response = api.request(http.MethodGet, "/search?q=screenshot&limit=5", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	files = response["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("file search should match attachment filename: %v", response)
	}
	fileHit = files[0].(map[string]any)
	attachments := fileHit["message"].(map[string]any)["attachments"].([]any)
	if attachments[0].(map[string]any)["name"] != "screenshot.png" {
		t.Fatalf("file search should return the filename-matched attachment: %v", fileHit)
	}

	status, response = api.request(http.MethodGet, "/search?q=Team&limit=5", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	messages = response["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("message search should match joined room name: %v", response)
	}
	messageHit = messages[0].(map[string]any)
	if messageHit["room"].(map[string]any)["id"] != teamRoomID {
		t.Fatalf("room-name message search should keep room context: %v", messageHit)
	}

	status, response = api.request(http.MethodGet, "/search?q=search_all_owner&limit=5", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	messages = response["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("message search should match sender username: %v", response)
	}
	messageHit = messages[0].(map[string]any)
	if messageHit["message"].(map[string]any)["sender"].(map[string]any)["username"] != "search_all_owner" {
		t.Fatalf("username message search should return sender context: %v", messageHit)
	}

	teamRID := teamRoom["rid"].(string)
	status, response = api.request(http.MethodGet, "/search?q="+teamRID+"&limit=5", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	messages = response["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("message search should match the complete room rid: %v", response)
	}
	messageHit = messages[0].(map[string]any)
	if messageHit["room"].(map[string]any)["id"] != teamRoomID {
		t.Fatalf("rid message search should keep room context: %v", messageHit)
	}

	status, response = api.request(http.MethodGet, "/rooms/search?q="+teamRID+"&limit=5", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	roomsByRID := response["rooms"].([]any)
	if len(roomsByRID) != 1 || roomsByRID[0].(map[string]any)["id"] != teamRoomID {
		t.Fatalf("room search should match the complete room rid: %v", response)
	}

	partialTeamRID := teamRID[:len(teamRID)-1]
	status, response = api.request(http.MethodGet, "/search?q="+partialTeamRID+"&limit=5", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	messages = response["messages"].([]any)
	if len(messages) != 0 {
		t.Fatalf("message search should not partially match room rid, got %d: %v", len(messages), response)
	}
	status, response = api.request(http.MethodGet, "/rooms/search?q="+partialTeamRID+"&limit=5", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["rooms"].([]any)); got != 0 {
		t.Fatalf("room search should not partially match room rid, got %d: %v", got, response)
	}

	ownerUID := owner.User["uid"].(string)
	status, response = api.request(http.MethodGet, "/search?q="+ownerUID+"&limit=5", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	messages = response["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("message search should match the complete sender uid: %v", response)
	}
	messageHit = messages[0].(map[string]any)
	if messageHit["message"].(map[string]any)["sender"].(map[string]any)["uid"] != ownerUID {
		t.Fatalf("uid message search should return sender context: %v", messageHit)
	}

	partialOwnerUID := ownerUID[:len(ownerUID)-1]
	status, response = api.request(http.MethodGet, "/search?q="+partialOwnerUID+"&limit=5", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	messages = response["messages"].([]any)
	if len(messages) != 0 {
		t.Fatalf("message search should not partially match sender uid, got %d: %v", len(messages), response)
	}
}

func TestSearchAllPaginatesMessagesIndependently(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("search_page_owner")
	discoverer := api.register("search_page_discoverer")
	room := api.createRoom(owner.Token, map[string]any{
		"name":        "Cursor needle-paging Room",
		"join_policy": "open",
	})
	roomID := room["id"].(string)
	api.createRoom(discoverer.Token, map[string]any{
		"name":        "needle-paging Public Room",
		"visibility":  "public",
		"join_policy": "open",
	})
	api.sendTypedMessage(owner.Token, roomID, "file", "needle-paging.pdf", []any{
		map[string]any{
			"type": "file",
			"name": "needle-paging.pdf",
			"asset": map[string]any{
				"id":        "asset_needle_paging",
				"url":       "/assets/needle-paging.pdf",
				"mime_type": "application/pdf",
				"filename":  "needle-paging.pdf",
			},
		},
	})

	oldest := api.sendMessage(owner.Token, roomID, "needle-paging oldest")
	middle := api.sendMessage(owner.Token, roomID, "needle-paging middle")
	newest := api.sendMessage(owner.Token, roomID, "needle-paging newest")
	for _, item := range []struct {
		message   map[string]any
		createdAt int64
	}{
		{message: oldest, createdAt: 1000},
		{message: middle, createdAt: 2000},
		{message: newest, createdAt: 3000},
	} {
		if _, err := api.db.Exec(`UPDATE messages SET created_at = ? WHERE id = ?`, item.createdAt, item.message["id"].(string)); err != nil {
			t.Fatalf("set message created_at: %v", err)
		}
	}

	status, response := api.request(http.MethodGet, "/search?q=needle-paging&limit=1&categories=messages,,unknown", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	messages := response["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["message"].(map[string]any)["id"] != newest["id"] {
		t.Fatalf("first message page should return newest hit only: %v", response)
	}
	cursors, ok := response["next_cursors"].(map[string]any)
	if !ok {
		t.Fatalf("search response missing next_cursors: %v", response)
	}
	messagesCursor, ok := cursors["messages"].(string)
	if !ok || messagesCursor == "" {
		t.Fatalf("limit=1 message search should return a message cursor: %v", response)
	}
	totals, ok := response["total_counts"].(map[string]any)
	if !ok {
		t.Fatalf("search response missing total_counts: %v", response)
	}
	if totals["messages"] != float64(3) {
		t.Fatalf("message total count should include all matching messages: %v", response)
	}
	for _, category := range []string{"my_rooms", "public_rooms", "files"} {
		if _, ok := cursors[category]; !ok {
			t.Fatalf("next_cursors should include %s: %v", category, response)
		}
		if cursors[category] != nil {
			t.Fatalf("unrequested category %s should not advance: %v", category, response)
		}
		items, ok := response[category].([]any)
		if !ok || len(items) != 0 {
			t.Fatalf("unrequested category %s should remain an empty array: %v", category, response)
		}
		if totals[category] != float64(0) {
			t.Fatalf("unrequested category %s should keep zero total count: %v", category, response)
		}
	}

	status, response = api.request(http.MethodGet, "/search?q=needle-paging&limit=1&categories=messages&messages_cursor="+messagesCursor, owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	messages = response["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["message"].(map[string]any)["id"] != middle["id"] {
		t.Fatalf("message cursor should return the next matching message only: %v", response)
	}
	cursors, ok = response["next_cursors"].(map[string]any)
	if !ok {
		t.Fatalf("cursor response missing next_cursors: %v", response)
	}
	if next, ok := cursors["messages"].(string); !ok || next == "" {
		t.Fatalf("second page should still expose the next message cursor: %v", response)
	}
	totals, ok = response["total_counts"].(map[string]any)
	if !ok {
		t.Fatalf("cursor response missing total_counts: %v", response)
	}
	if totals["messages"] != float64(3) {
		t.Fatalf("cursor response should keep full message total count: %v", response)
	}
	for _, category := range []string{"my_rooms", "public_rooms", "files"} {
		if _, ok := cursors[category]; !ok {
			t.Fatalf("cursor response next_cursors should include %s: %v", category, response)
		}
		if cursors[category] != nil {
			t.Fatalf("cursor request should not advance unrequested category %s: %v", category, response)
		}
		items, ok := response[category].([]any)
		if !ok || len(items) != 0 {
			t.Fatalf("cursor request should keep unrequested category %s empty: %v", category, response)
		}
		if totals[category] != float64(0) {
			t.Fatalf("cursor request should keep unrequested category %s zero total count: %v", category, response)
		}
	}

	status, response = api.request(http.MethodGet, "/search?q=needle-paging&limit=1&categories=unknown,,", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	for _, category := range []string{"my_rooms", "public_rooms", "messages", "files"} {
		items, ok := response[category].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("search with no valid categories should fall back to all categories; %s got %v in %v", category, response[category], response)
		}
	}
	totals, ok = response["total_counts"].(map[string]any)
	if !ok {
		t.Fatalf("fallback response missing total_counts: %v", response)
	}
	wantTotals := map[string]float64{
		"my_rooms":     1,
		"public_rooms": 1,
		"messages":     3,
		"files":        1,
	}
	for category, want := range wantTotals {
		if totals[category] != want {
			t.Fatalf("fallback total_counts[%s] got %v want %v in %v", category, totals[category], want, response)
		}
	}
}

func TestUserSearchIncludesSuperuserFlag(t *testing.T) {
	api := newAPIHarness(t)
	super := api.login("GANG", "64n9-Ch47")
	normal := api.register("search_normal")

	status, response := api.request(http.MethodGet, "/users/search?q=GANG&limit=20", normal.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	users := response["users"].([]any)
	if len(users) == 0 || users[0].(map[string]any)["id"] != super.User["id"] || users[0].(map[string]any)["is_superuser"] != true {
		t.Fatalf("user search should include superuser flag: %v", response)
	}

	status, response = api.request(http.MethodGet, "/users/search?q=search_normal&limit=20", normal.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	users = response["users"].([]any)
	if len(users) == 0 || users[0].(map[string]any)["id"] != normal.User["id"] || users[0].(map[string]any)["is_superuser"] != false {
		t.Fatalf("user search should include normal superuser flag: %v", response)
	}

	normalUID := normal.User["uid"].(string)
	status, response = api.request(http.MethodGet, "/users/search?q="+normalUID+"&limit=20", normal.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	users = response["users"].([]any)
	if len(users) != 1 || users[0].(map[string]any)["id"] != normal.User["id"] {
		t.Fatalf("user search should match the complete uid: %v", response)
	}

	partialNormalUID := normalUID[:len(normalUID)-1]
	status, response = api.request(http.MethodGet, "/users/search?q="+partialNormalUID+"&limit=20", normal.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	users = response["users"].([]any)
	if len(users) != 0 {
		t.Fatalf("user search should not partially match uid, got %d: %v", len(users), response)
	}

	owner := api.register("search_room_owner")
	target := api.register("search_room_target")
	room := api.createRoom(owner.Token, map[string]any{"name": "NebulaRoomNeedle", "join_policy": "open"})
	roomID := room["id"].(string)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", target.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if _, err := api.db.Exec(
		`UPDATE room_memberships SET room_display_name = ? WHERE room_id = ? AND user_id = ?`,
		"NebulaRoomAlias", roomID, target.User["id"].(string),
	); err != nil {
		t.Fatalf("update target room display name: %v", err)
	}
	for _, q := range []string{"NebulaRoomNeedle", "NebulaRoomAlias"} {
		status, response = api.request(http.MethodGet, "/users/search?q="+q+"&limit=20", normal.Token, nil)
		api.requireStatus(status, http.StatusOK, response)
		users = response["users"].([]any)
		if len(users) != 0 {
			t.Fatalf("user search should not match room names for %q, got %d: %v", q, len(users), response)
		}
	}
}
