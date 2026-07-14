package chat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestUTF8JSONRoundTripAndHeaders(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("utf8_owner")

	room := api.createRoom(owner.Token, map[string]any{
		"name":        "中文房间",
		"join_policy": "open",
	})
	if room["name"] != "中文房间" {
		t.Fatalf("room name should preserve UTF-8 Chinese: %v", room)
	}

	rec := api.rawRequest(http.MethodPost, "/rooms/"+room["id"].(string)+"/messages", owner.Token, map[string]any{
		"client_message_id": "utf8_msg_1",
		"body":              "你好，世界",
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("send message status=%d body=%q", rec.Code, rec.Body.String())
	}
	if contentType := strings.ToLower(rec.Header().Get("Content-Type")); !strings.Contains(contentType, "charset=utf-8") {
		t.Fatalf("JSON response should declare UTF-8 charset, got %q", rec.Header().Get("Content-Type"))
	}

	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode message response: %v body=%q", err, rec.Body.String())
	}
	message := response["message"].(map[string]any)
	if message["body"] != "你好，世界" {
		t.Fatalf("message body should preserve UTF-8 Chinese: %v", message)
	}
}

func TestLastMessagePreviewUsesAttachmentLabels(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("last_preview_owner")
	room := api.createRoom(owner.Token, map[string]any{"name": "Preview Room", "join_policy": "open"})
	roomID := room["id"].(string)

	assertLastPreview := func(want string) {
		t.Helper()
		status, response := api.request(http.MethodGet, "/rooms", owner.Token, nil)
		api.requireStatus(status, http.StatusOK, response)
		room := roomCardByID(t, response, roomID)
		last := room["last_message"].(map[string]any)
		if last["body_preview"] != want {
			t.Fatalf("last_message preview mismatch: got %v want %s room=%v", last["body_preview"], want, room)
		}
	}

	api.sendTypedMessage(owner.Token, roomID, "audio", "voice_1.m4a", []any{
		map[string]any{
			"type":        "audio",
			"name":        "voice_1.m4a",
			"duration_ms": float64(15000),
			"asset": map[string]any{
				"id":        "asset_voice",
				"url":       "/assets/voice_1.m4a",
				"mime_type": "audio/mp4",
				"filename":  "voice_1.m4a",
			},
		},
	})
	assertLastPreview(`[语音] 15"`)

	api.sendTypedMessage(owner.Token, roomID, "file", "screenshot.png", []any{
		map[string]any{
			"type": "file",
			"name": "screenshot.png",
			"asset": map[string]any{
				"id":        "asset_image",
				"url":       "/assets/screenshot.png",
				"mime_type": "image/png",
				"filename":  "screenshot.png",
			},
		},
	})
	assertLastPreview("[图片] screenshot.png")

	api.sendTypedMessage(owner.Token, roomID, "file", "report.pdf", []any{
		map[string]any{
			"type": "file",
			"name": "report.pdf",
			"asset": map[string]any{
				"id":        "asset_file",
				"url":       "/assets/report.pdf",
				"mime_type": "application/pdf",
				"filename":  "report.pdf",
			},
		},
	})
	assertLastPreview("[文件] report.pdf")

	api.sendTypedMessage(owner.Token, roomID, "sticker", "[wave]", []any{
		map[string]any{
			"type": "sticker",
			"name": "wave",
			"asset": map[string]any{
				"id":        "asset_sticker",
				"url":       "/assets/wave.webp",
				"mime_type": "image/webp",
				"filename":  "wave.webp",
			},
		},
	})
	assertLastPreview("[表情] wave")
}

func TestLastMessagePreviewUsesRoomDisplayName(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("last_preview_room_name_owner")
	room := api.createRoom(owner.Token, map[string]any{"name": "Preview Name Room", "join_policy": "open"})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPatch, "/rooms/"+roomID+"/me", owner.Token, map[string]any{
		"room_display_name": "Owner In Room",
	})
	api.requireStatus(status, http.StatusOK, response)

	api.sendMessage(owner.Token, roomID, "hello from room profile")

	status, response = api.request(http.MethodGet, "/rooms", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	roomCard := roomCardByID(t, response, roomID)
	last := roomCard["last_message"].(map[string]any)
	if last["sender_display_name"] != "Owner In Room" {
		t.Fatalf("last_message should use room display name, got %v", last)
	}
}

func TestLastMessagePreviewIncludesSystemType(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("system_preview_owner")
	member := api.register("system_preview_member")
	room := api.createRoom(owner.Token, map[string]any{"name": "System Preview", "join_policy": "open"})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/rooms", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	card := roomCardByID(t, response, roomID)
	last := card["last_message"].(map[string]any)
	if last["type"] != systemMessageType {
		t.Fatalf("system last_message should include type: %v", last)
	}
	if last["sender_display_name"] != "system_preview_member" || last["body_preview"] != "加入了房间" {
		t.Fatalf("system last_message should carry subject and detail: %v", last)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID, owner.Token, map[string]any{
		"name": "Renamed Preview",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/rooms", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	card = roomCardByID(t, response, roomID)
	last = card["last_message"].(map[string]any)
	if last["sender_display_name"] != "" || last["body_preview"] != "房间名称 被 system_preview_owner 修改为 Renamed Preview" {
		t.Fatalf("room name system last_message should match chat rendering order: %v", last)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID, owner.Token, map[string]any{
		"description": "Preview bio",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/rooms", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	card = roomCardByID(t, response, roomID)
	last = card["last_message"].(map[string]any)
	if last["sender_display_name"] != "" || last["body_preview"] != "房间简介 被 system_preview_owner 修改为 Preview bio" {
		t.Fatalf("room description system last_message should match chat rendering order: %v", last)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID, owner.Token, map[string]any{
		"visibility": "private",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/rooms", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	card = roomCardByID(t, response, roomID)
	last = card["last_message"].(map[string]any)
	if last["sender_display_name"] != "" || last["body_preview"] != "房间可见性 被 system_preview_owner 修改为 私密" {
		t.Fatalf("room visibility system last_message should match chat rendering order: %v", last)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID, owner.Token, map[string]any{
		"join_policy": "closed",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/rooms", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	card = roomCardByID(t, response, roomID)
	last = card["last_message"].(map[string]any)
	if last["sender_display_name"] != "" || last["body_preview"] != "房间加入方式 被 system_preview_owner 修改为 关闭" {
		t.Fatalf("room join policy system last_message should match chat rendering order: %v", last)
	}
}

func TestLastMessagePreviewTreatsRemovedLatestMessageAsSystem(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("removed_preview_owner")
	member := api.register("removed_preview_member")
	peer := api.register("removed_preview_peer")
	room := api.createRoom(owner.Token, map[string]any{"name": "Removed Preview", "join_policy": "open"})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", peer.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/me", member.Token, map[string]any{
		"room_display_name": "Member In Removed Room",
	})
	api.requireStatus(status, http.StatusOK, response)

	recalled := api.sendMessage(member.Token, roomID, "message to recall")
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/messages/"+recalled["id"].(string)+"/recall", member.Token, map[string]any{
		"reason": "test",
	})
	api.requireStatus(status, http.StatusOK, response)
	recalledPayload := response["message"].(map[string]any)
	if recalledPayload["body"] != "message to recall" {
		t.Fatalf("recalling sender should receive original recalled text: %v", recalledPayload)
	}
	recalledBy := recalledPayload["recalled_by"].(map[string]any)
	if recalledBy["room_display_name"] != "Member In Removed Room" {
		t.Fatalf("recall response should use actor room display name: %v", recalledBy)
	}

	ownerMessages := listRoomMessages(t, api, owner.Token, roomID)
	if ownerMessages[len(ownerMessages)-1]["body"] != "message to recall" {
		t.Fatalf("higher role should see recalled text body: %v", ownerMessages[len(ownerMessages)-1])
	}
	listedRecalledBy := ownerMessages[len(ownerMessages)-1]["recalled_by"].(map[string]any)
	if listedRecalledBy["room_display_name"] != "Member In Removed Room" {
		t.Fatalf("listed recalled message should use actor room display name: %v", listedRecalledBy)
	}
	peerMessages := listRoomMessages(t, api, peer.Token, roomID)
	if peerMessages[len(peerMessages)-1]["body"] != "" {
		t.Fatalf("same role peer should not see recalled text body: %v", peerMessages[len(peerMessages)-1])
	}

	status, response = api.request(http.MethodGet, "/rooms", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	card := roomCardByID(t, response, roomID)
	last := card["last_message"].(map[string]any)
	if last["type"] != systemMessageType || last["sender_display_name"] != "" || last["body_preview"] != "Member In Removed Room 撤回了一条消息" {
		t.Fatalf("recalled last_message should match removed system row: %v", last)
	}

	deleted := api.sendMessage(member.Token, roomID, "message to delete")
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/messages/"+deleted["id"].(string)+"/force-delete", owner.Token, map[string]any{
		"confirm": true,
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/rooms", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	card = roomCardByID(t, response, roomID)
	last = card["last_message"].(map[string]any)
	if last["type"] != systemMessageType || last["sender_display_name"] != "" || last["body_preview"] != "removed_preview_owner 删除了一条消息" {
		t.Fatalf("force-deleted last_message should match removed system row: %v", last)
	}
}

func TestHistoricalLiveSystemMessagesAreHidden(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("hidden_live_owner")
	member := api.register("hidden_live_member")
	room := api.createRoom(owner.Token, map[string]any{"name": "Hidden Live", "join_policy": "open"})
	roomID := room["id"].(string)
	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	visible := api.sendMessage(owner.Token, roomID, "visible before live")
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/read", member.Token, map[string]any{
		"last_read_message_id": visible["id"],
	})
	api.requireStatus(status, http.StatusOK, response)

	handler := &Handler{DB: api.db}
	if err := handler.appendSystemMessage(roomID, systemMessageSpec{
		Event:  systemEventLiveJoined,
		UserID: owner.User["id"].(string),
	}); err != nil {
		t.Fatalf("append historical live join: %v", err)
	}
	if err := handler.appendSystemMessage(roomID, systemMessageSpec{
		Event:  systemEventLiveLeft,
		UserID: owner.User["id"].(string),
	}); err != nil {
		t.Fatalf("append historical live leave: %v", err)
	}

	messages := listRoomMessages(t, api, member.Token, roomID)
	if hasSystemMessage(t, messages, systemEventLiveJoined, owner.User["id"].(string)) ||
		hasSystemMessage(t, messages, systemEventLiveLeft, owner.User["id"].(string)) {
		t.Fatalf("historical live system messages should be hidden: %v", messages)
	}

	status, response = api.request(http.MethodGet, "/rooms", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	card := roomCardByID(t, response, roomID)
	last := card["last_message"].(map[string]any)
	if last["body_preview"] != "visible before live" {
		t.Fatalf("last_message should ignore hidden live system messages: %v", last)
	}
	if card["unread_count"] != float64(0) {
		t.Fatalf("hidden live system messages should not increment unread count: %v", card)
	}
}

func TestRoomMessageHistoryFiltersAndDeletesOnlyForCurrentViewer(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("history_owner")
	member := api.register("history_member")
	room := api.createRoom(owner.Token, map[string]any{
		"name":        "Message History",
		"join_policy": "open",
	})
	roomID := room["id"].(string)
	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	linkMessage := api.sendMessage(owner.Token, roomID, "visit https://example.test/history")
	memberMessage := api.sendMessage(member.Token, roomID, "member-only-history-needle")
	imageMessage := api.sendTypedMessage(owner.Token, roomID, "file", "history-image.png", []any{
		map[string]any{
			"type": "file",
			"name": "history-image.png",
			"asset": map[string]any{
				"id":        "asset_history_image",
				"url":       "/assets/history-image.png",
				"mime_type": "image/png",
				"filename":  "history-image.png",
			},
		},
	})
	fileMessage := api.sendTypedMessage(owner.Token, roomID, "file", "history-file.pdf", []any{
		map[string]any{
			"type": "file",
			"name": "history-file.pdf",
			"asset": map[string]any{
				"id":        "asset_history_file",
				"url":       "/assets/history-file.pdf",
				"mime_type": "application/pdf",
				"filename":  "history-file.pdf",
			},
		},
	})
	voiceMessage := api.sendTypedMessage(owner.Token, roomID, "audio", "voice_history_current.m4a", []any{
		map[string]any{
			"type":        "audio",
			"name":        "voice_history_current.m4a",
			"duration_ms": float64(1200),
			"asset": map[string]any{
				"id":        "asset_history_voice_current",
				"url":       "/assets/voice_history_current.m4a",
				"mime_type": "audio/mp4",
				"filename":  "voice_history_current.m4a",
			},
		},
	})
	legacyVoiceMessage := api.sendTypedMessage(owner.Token, roomID, "file", "voice_history_legacy.m4a", []any{
		map[string]any{
			"type":        "file",
			"name":        "voice_history_legacy.m4a",
			"duration_ms": float64(2200),
			"asset": map[string]any{
				"id":        "asset_history_voice_legacy",
				"url":       "/assets/voice_history_legacy.m4a",
				"mime_type": "audio/mp4",
				"filename":  "voice_history_legacy.m4a",
			},
		},
	})
	stickerMessage := api.sendTypedMessage(owner.Token, roomID, "sticker", "[history-wave]", []any{
		map[string]any{
			"type": "sticker",
			"name": "history-wave",
			"asset": map[string]any{
				"id":        "asset_history_sticker",
				"url":       "/assets/history-wave.webp",
				"mime_type": "image/webp",
				"filename":  "history-wave.webp",
			},
		},
	})
	linkID := linkMessage["id"].(string)
	memberMessageID := memberMessage["id"].(string)

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/message-history?category=links", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	assertHistoryContainsExactly(t, response, linkID)

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/message-history?category=voice", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	assertHistoryContainsIDs(t, response,
		voiceMessage["id"].(string),
		legacyVoiceMessage["id"].(string),
	)

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/message-history?category=images", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	assertHistoryContainsExactly(t, response, imageMessage["id"].(string))

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/message-history?category=files", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	assertHistoryContainsExactly(t, response, fileMessage["id"].(string))

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/message-history?category=stickers", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	assertHistoryContainsExactly(t, response, stickerMessage["id"].(string))

	status, response = api.request(
		http.MethodGet,
		"/rooms/"+roomID+"/message-history?sender_user_id="+member.User["id"].(string),
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	if !historyContains(response, memberMessageID) {
		t.Fatalf("member filter should contain the member's text message: %v", response)
	}
	for _, raw := range response["messages"].([]any) {
		messagePayload := raw.(map[string]any)
		sender := messagePayload["sender"].(map[string]any)
		if sender["id"] != member.User["id"] {
			t.Fatalf("member filter returned another sender: %v", messagePayload)
		}
	}

	status, response = api.request(
		http.MethodGet,
		"/rooms/"+roomID+"/message-history?query=member-only-history-needle",
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	assertHistoryContainsExactly(t, response, memberMessageID)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/message-history/hide", owner.Token, map[string]any{
		"message_ids": []string{linkID},
		"confirm":     true,
	})
	api.requireStatus(status, http.StatusOK, response)
	if response["deleted_count"] != float64(1) {
		t.Fatalf("delete count mismatch: %v", response)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/message-history", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if historyContains(response, linkID) {
		t.Fatalf("owner history should hide deleted record: %v", response)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/message-history", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if !historyContains(response, linkID) {
		t.Fatalf("another member should still see the record: %v", response)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/messages?limit=100", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if !historyContains(response, linkID) {
		t.Fatalf("deleting a history record must not delete the room message: %v", response)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/message-history?category=unknown", owner.Token, nil)
	api.requireStatus(status, http.StatusBadRequest, response)
}

func TestMessageRecallPolicies(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("recall_owner")
	joiner := api.register("recall_joiner")
	room := api.createRoom(owner.Token, map[string]any{"name": "Recall Room", "join_policy": "open"})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", joiner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/settings", owner.Token, map[string]any{"message_recall_policy": "disabled"})
	api.requireStatus(status, http.StatusOK, response)
	blocked := api.sendMessage(joiner.Token, roomID, "blocked recall")
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/messages/"+blocked["id"].(string)+"/recall", joiner.Token, map[string]any{"reason": "test"})
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/settings", owner.Token, map[string]any{"message_recall_policy": "admin_approval"})
	api.requireStatus(status, http.StatusOK, response)
	candidate := api.sendMessage(joiner.Token, roomID, "recall with approval")
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/messages/"+candidate["id"].(string)+"/recall", joiner.Token, map[string]any{"reason": "test"})
	api.requireStatus(status, http.StatusAccepted, response)
	recallRequest := response["recall_request"].(map[string]any)
	if recallRequest["status"] != "pending" {
		t.Fatalf("recall request should be pending: %v", recallRequest)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/messages?limit=20", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	beforeApprove := findMessage(t, response, candidate["id"].(string))
	if beforeApprove["is_recalled"] != false {
		t.Fatalf("message should not be recalled before approval: %v", beforeApprove)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/message-recall-requests/"+recallRequest["id"].(string), owner.Token, map[string]any{"decision": "approve"})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/messages?limit=20", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	afterApprove := findMessage(t, response, candidate["id"].(string))
	if afterApprove["is_recalled"] != true {
		t.Fatalf("message should be recalled after approval: %v", afterApprove)
	}
}
