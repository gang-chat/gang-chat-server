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

func TestMessageQuoteUsesImmutableSingleLevelSnapshot(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("quote_owner")
	room := api.createRoom(owner.Token, map[string]any{
		"name":        "Quote Room",
		"join_policy": "open",
	})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPatch, "/rooms/"+roomID+"/me", owner.Token, map[string]any{
		"room_display_name": "发送时房内名",
	})
	api.requireStatus(status, http.StatusOK, response)
	original := api.sendTypedMessage(owner.Token, roomID, "file", "report.pdf", []any{
		map[string]any{
			"type": "file",
			"name": "report.pdf",
			"asset": map[string]any{
				"id":        "asset_quote_file",
				"url":       "/assets/report.pdf",
				"mime_type": "application/pdf",
				"filename":  "report.pdf",
			},
		},
	})

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/me", owner.Token, map[string]any{
		"room_display_name": "修改后的房内名",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/messages", owner.Token, map[string]any{
		"client_message_id": "quote_reply_1",
		"body":              "第一次回复",
		"quote_message_id":  original["id"],
	})
	api.requireStatus(status, http.StatusCreated, response)
	firstReply := response["message"].(map[string]any)
	quote := firstReply["quote"].(map[string]any)
	if quote["message_id"] != original["id"] ||
		quote["sender_display_name"] != "发送时房内名" ||
		quote["body"] != "[文件] report.pdf" || quote["created_at"] != original["created_at"] {
		t.Fatalf("quote should snapshot original file preview, name, and time: %v", quote)
	}
	if _, exists := quote["preview_attachment"]; exists {
		t.Fatalf("non-image file quote should stay text-only: %v", quote)
	}

	image := api.sendTypedMessage(owner.Token, roomID, "file", "photo.png", []any{
		map[string]any{
			"type": "file",
			"name": "photo.png",
			"asset": map[string]any{
				"id":            "asset_quote_image",
				"url":           "/assets/photo.png",
				"thumbnail_url": "/assets/photo-thumb.png",
				"mime_type":     "image/png",
				"filename":      "photo.png",
			},
		},
	})
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/messages", owner.Token, map[string]any{
		"client_message_id": "quote_image_reply",
		"body":              "图片回复",
		"quote_message_id":  image["id"],
	})
	api.requireStatus(status, http.StatusCreated, response)
	imageQuote := response["message"].(map[string]any)["quote"].(map[string]any)
	preview := imageQuote["preview_attachment"].(map[string]any)
	if preview["type"] != "file" || preview["name"] != "photo.png" {
		t.Fatalf("image quote should carry a preview attachment snapshot: %v", imageQuote)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/messages", owner.Token, map[string]any{
		"client_message_id": "quote_reply_2",
		"body":              "第二次回复",
		"quote_message_ids": []any{firstReply["id"], firstReply["id"], image["id"]},
	})
	api.requireStatus(status, http.StatusCreated, response)
	secondReply := response["message"].(map[string]any)
	nestedQuote := secondReply["quote"].(map[string]any)
	if nestedQuote["message_id"] != firstReply["id"] || nestedQuote["body"] != "第一次回复" {
		t.Fatalf("quoting a quoted message must snapshot only its own body: %v", nestedQuote)
	}
	multipleQuotes := secondReply["quotes"].([]any)
	if len(multipleQuotes) != 2 ||
		multipleQuotes[0].(map[string]any)["message_id"] != firstReply["id"] ||
		multipleQuotes[1].(map[string]any)["message_id"] != image["id"] {
		t.Fatalf("multiple quotes should preserve selection order and remove duplicates: %v", multipleQuotes)
	}

	status, response = api.request(http.MethodGet, "/rooms", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	last := roomCardByID(t, response, roomID)["last_message"].(map[string]any)
	if last["body_preview"] != "[引用] 第二次回复" {
		t.Fatalf("room preview should identify quoted message: %v", last)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/messages/"+firstReply["id"].(string)+"/recall", owner.Token, map[string]any{
		"reason": "test snapshot retention",
	})
	api.requireStatus(status, http.StatusOK, response)
	messages := listRoomMessages(t, api, owner.Token, roomID)
	retained := findMessageInList(t, messages, secondReply["id"].(string))
	retainedQuote := retained["quote"].(map[string]any)
	if retainedQuote["body"] != "第一次回复" {
		t.Fatalf("later recall must not mutate an existing quote snapshot: %v", retainedQuote)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/messages", owner.Token, map[string]any{
		"client_message_id": "quote_recalled_source",
		"body":              "不应发送",
		"quote_message_id":  firstReply["id"],
	})
	if status != http.StatusBadRequest || response["code"] != "validation_failed" {
		t.Fatalf("recalled source should not be quotable: status=%d response=%v", status, response)
	}
}

func TestQuotedSystemMessageSnapshotOmitsHeaderSenderAndKeepsBodySubject(t *testing.T) {
	msg := message{
		Type: systemMessageType,
		Body: "加入了房间",
		Sender: userSummary{
			Username:    "system_subject",
			DisplayName: "System Subject",
		},
		Attachments: []any{
			map[string]any{
				"type":  systemMessageType,
				"event": systemEventRoomMemberJoined,
				"user": map[string]any{
					"username":          "system_subject",
					"display_name":      "System Subject",
					"room_display_name": "Room Subject",
				},
			},
		},
	}
	if got := quotedMessageSenderName(msg); got != "" {
		t.Fatalf("system message quote should omit its sender name, got %q", got)
	}
	if got := quotedMessageBodySnapshot(msg); got != "Room Subject 加入了房间" {
		t.Fatalf("system message quote should retain its subject in the body, got %q", got)
	}

	roleChanged := message{
		Type: systemMessageType,
		Attachments: []any{
			map[string]any{
				"type":      systemMessageType,
				"event":     systemEventRoomRoleChanged,
				"from_role": "member",
				"to_role":   "admin",
				"target": map[string]any{
					"room_display_name": "Room Target",
				},
				"actor": map[string]any{
					"room_display_name": "Room Owner",
				},
			},
		},
	}
	if got := quotedMessageBodySnapshot(roleChanged); got != "Room Target 被 Room Owner 晋升为 管理员" {
		t.Fatalf("role-change quote should preserve its rendered participants, got %q", got)
	}

	descriptionChanged := message{
		Type: systemMessageType,
		Attachments: []any{
			map[string]any{
				"type":      systemMessageType,
				"event":     systemEventRoomBioChanged,
				"new_value": "New description",
				"user": map[string]any{
					"room_display_name": "Room Owner",
				},
			},
		},
	}
	if got := quotedMessageBodySnapshot(descriptionChanged); got != "房间简介 被 Room Owner 修改为\nNew description" {
		t.Fatalf("room-profile quote should preserve its rendered actor and value, got %q", got)
	}
}

func TestQuotedSystemMessageKeepsRenderedBodyAfterSending(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("system_quote_owner")
	member := api.register("system_quote_member")
	room := api.createRoom(owner.Token, map[string]any{
		"name":        "System Quote Room",
		"join_policy": "open",
	})
	roomID := room["id"].(string)
	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	source := requireSystemMessage(
		t,
		listRoomMessages(t, api, owner.Token, roomID),
		systemEventRoomMemberJoined,
		member.User["id"].(string),
	)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/messages", owner.Token, map[string]any{
		"client_message_id": "system_quote_reply",
		"body":              "reply",
		"quote_message_id":  source["id"],
	})
	api.requireStatus(status, http.StatusCreated, response)
	quote := response["message"].(map[string]any)["quote"].(map[string]any)
	if quote["sender_display_name"] != "" ||
		quote["body"] != member.User["username"].(string)+" 加入了房间" {
		t.Fatalf("sent system quote should show time-only header and retain the subject body: %v", quote)
	}
}

func findMessageInList(t *testing.T, messages []map[string]any, messageID string) map[string]any {
	t.Helper()
	for _, message := range messages {
		if message["id"] == messageID {
			return message
		}
	}
	t.Fatalf("message %s not found: %v", messageID, messages)
	return nil
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
