package chat

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/auth"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	"github.com/zhuangkaiyi/gang-chat/server/internal/db"
	"github.com/zhuangkaiyi/gang-chat/server/internal/eventbus"
	"github.com/zhuangkaiyi/gang-chat/server/internal/idgen"
)

type apiHarness struct {
	t      *testing.T
	router *gin.Engine
	db     *sql.DB
	live   *fakeLiveController
	bus    *eventbus.Bus
	assets string
	cfg    *config.Config
}

// fakeLiveController records the LiveKit media-control calls moderation makes,
// so tests can assert the server drove the media session (not just the DB).
type fakeLiveController struct {
	removed    []string // "room/identity"
	publishSet []string // "room/identity=true|false"
	micMuted   []string // "room/identity=true|false"
	removeErr  error
}

func (f *fakeLiveController) RemoveParticipant(room, identity string) error {
	f.removed = append(f.removed, room+"/"+identity)
	return f.removeErr
}

func (f *fakeLiveController) SetCanPublish(room, identity string, canPublish bool) error {
	f.publishSet = append(f.publishSet, room+"/"+identity+"="+strconv.FormatBool(canPublish))
	return nil
}

func (f *fakeLiveController) MuteMicrophone(room, identity string, muted bool) error {
	f.micMuted = append(f.micMuted, room+"/"+identity+"="+strconv.FormatBool(muted))
	return nil
}

type testSession struct {
	Token string
	User  map[string]any
}

func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	root := t.TempDir()
	cfg := &config.Config{
		JWTSecret:              "test-secret",
		AccessTokenTTLSeconds:  900,
		RefreshTokenTTLSeconds: 2592000,
		LoginMaxAttempts:       5,
		LoginWindowSeconds:     900,
		AssetDir:               filepath.Join(root, "assets"),
		LiveKitHost:            "http://localhost:7880",
	}
	pool := db.Connect(filepath.Join(root, "gang-chat-test.db"))
	t.Cleanup(func() { _ = pool.Close() })

	router := gin.New()
	api := router.Group("/api/v1")
	auth.RegisterRoutes(api, pool, cfg)

	authMW := &auth.AuthMiddleware{DB: pool, JWTSecret: cfg.JWTSecret}
	chatGroup := api.Group("")
	chatGroup.Use(authMW.Handle)
	live := &fakeLiveController{}
	bus := eventbus.New()
	RegisterRoutes(chatGroup, pool, cfg, bus, live, nil)

	return &apiHarness{
		t:      t,
		router: router,
		db:     pool,
		live:   live,
		bus:    bus,
		assets: cfg.AssetDir,
		cfg:    cfg,
	}
}

func (h *apiHarness) request(method, path, token string, body any) (int, map[string]any) {
	return h.requestWithHeaders(method, path, token, body, nil)
}

func (h *apiHarness) requestWithHeaders(method, path, token string, body any, headers map[string]string) (int, map[string]any) {
	h.t.Helper()
	rec := h.rawRequest(method, path, token, body, headers)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			h.t.Fatalf("%s %s returned invalid JSON: %v; body=%q", method, path, err, rec.Body.String())
		}
	}
	return rec.Code, decoded
}

func (h *apiHarness) rawRequest(method, path, token string, body any, headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal request body: %v", err)
		}
	}
	req := httptest.NewRequest(method, "/api/v1"+path, bytes.NewReader(payload))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func (h *apiHarness) requireStatus(status, want int, response map[string]any) {
	h.t.Helper()
	if status != want {
		h.t.Fatalf("unexpected status: got %d want %d response=%v", status, want, response)
	}
}

func (h *apiHarness) register(username string) testSession {
	h.t.Helper()
	status, response := h.request(http.MethodPost, "/auth/register", "", map[string]any{
		"username": username,
		"email":    username + "@example.com",
		"password": "correct horse battery staple",
	})
	h.requireStatus(status, http.StatusCreated, response)
	user, ok := response["user"].(map[string]any)
	if !ok {
		h.t.Fatalf("register response missing user: %v", response)
	}
	token, ok := response["access_token"].(string)
	if !ok || token == "" {
		h.t.Fatalf("register response missing access token: %v", response)
	}
	return testSession{Token: token, User: user}
}

func (h *apiHarness) login(login, password string) testSession {
	h.t.Helper()
	status, response := h.request(http.MethodPost, "/auth/login", "", map[string]any{
		"login":    login,
		"password": password,
	})
	h.requireStatus(status, http.StatusOK, response)
	user, ok := response["user"].(map[string]any)
	if !ok {
		h.t.Fatalf("login response missing user: %v", response)
	}
	token, ok := response["access_token"].(string)
	if !ok || token == "" {
		h.t.Fatalf("login response missing access token: %v", response)
	}
	return testSession{Token: token, User: user}
}

func (h *apiHarness) createRoom(token string, body map[string]any) map[string]any {
	h.t.Helper()
	status, response := h.request(http.MethodPost, "/rooms", token, body)
	h.requireStatus(status, http.StatusCreated, response)
	room, ok := response["room"].(map[string]any)
	if !ok {
		h.t.Fatalf("create room response missing room: %v", response)
	}
	return room
}

func (h *apiHarness) uploadMultipart(path, token, filename, contentType, purpose string, data []byte) (int, map[string]any) {
	h.t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if purpose != "" {
		if err := writer.WriteField("purpose", purpose); err != nil {
			h.t.Fatalf("write purpose: %v", err)
		}
	}
	headers := make(textproto.MIMEHeader)
	headers.Set("Content-Disposition", `form-data; name="file"; filename="`+strings.ReplaceAll(filename, `"`, `\"`)+`"`)
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	part, err := writer.CreatePart(headers)
	if err != nil {
		h.t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		h.t.Fatalf("write form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		h.t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1"+path, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			h.t.Fatalf("%s returned invalid JSON: %v; body=%q", path, err, rec.Body.String())
		}
	}
	return rec.Code, decoded
}

func memberByUserID(t *testing.T, response map[string]any, userID string) map[string]any {
	t.Helper()
	items, ok := response["members"].([]any)
	if !ok {
		t.Fatalf("members response missing members: %v", response)
	}
	for _, item := range items {
		member, ok := item.(map[string]any)
		if !ok {
			continue
		}
		user, ok := member["user"].(map[string]any)
		if ok && user["id"] == userID {
			return member
		}
	}
	t.Fatalf("member %s not found: %v", userID, response)
	return nil
}

func hasStickerNames(pack map[string]any, names ...string) bool {
	stickers, ok := pack["stickers"].([]any)
	if !ok {
		return false
	}
	seen := make(map[string]int, len(stickers))
	for _, item := range stickers {
		sticker, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := sticker["name"].(string)
		seen[name]++
	}
	for _, name := range names {
		if seen[name] == 0 {
			return false
		}
		seen[name]--
	}
	return true
}

func participantByUserID(t *testing.T, live map[string]any, userID string) map[string]any {
	t.Helper()
	items, ok := live["participants"].([]any)
	if !ok {
		t.Fatalf("live response missing participants: %v", live)
	}
	for _, item := range items {
		participant, ok := item.(map[string]any)
		if !ok {
			continue
		}
		user, ok := participant["user"].(map[string]any)
		if ok && user["id"] == userID {
			return participant
		}
	}
	t.Fatalf("participant %s not found: %v", userID, live)
	return nil
}

func (h *apiHarness) sendMessage(token, roomID, body string) map[string]any {
	h.t.Helper()
	status, response := h.request(http.MethodPost, "/rooms/"+roomID+"/messages", token, map[string]any{
		"client_message_id": "test_msg_" + idgen.New("client"),
		"body":              body,
	})
	h.requireStatus(status, http.StatusCreated, response)
	message, ok := response["message"].(map[string]any)
	if !ok {
		h.t.Fatalf("send message response missing message: %v", response)
	}
	return message
}

func (h *apiHarness) sendTypedMessage(token, roomID, messageType, body string, attachments []any) map[string]any {
	h.t.Helper()
	status, response := h.request(http.MethodPost, "/rooms/"+roomID+"/messages", token, map[string]any{
		"client_message_id": "test_msg_" + idgen.New("client"),
		"type":              messageType,
		"body":              body,
		"attachments":       attachments,
	})
	h.requireStatus(status, http.StatusCreated, response)
	message, ok := response["message"].(map[string]any)
	if !ok {
		h.t.Fatalf("send typed message response missing message: %v", response)
	}
	return message
}

func parseNumericID(t *testing.T, value any) int64 {
	t.Helper()
	raw, ok := value.(string)
	if !ok || raw == "" {
		t.Fatalf("expected numeric id string, got %#v", value)
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("numeric id is not parseable: %q", raw)
	}
	return n
}

func assertRoomInvitesKeepDeletedRooms(t *testing.T, pool *sql.DB) {
	t.Helper()
	rows, err := pool.Query(`PRAGMA foreign_key_list(room_invites)`)
	if err != nil {
		t.Fatalf("read room_invites foreign keys: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, seq int
		var tableName, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &tableName, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatalf("scan room_invites foreign key: %v", err)
		}
		if tableName == "rooms" && from == "room_id" {
			t.Fatalf("room_invites.room_id must not cascade with rooms, on_delete=%s", onDelete)
		}
		if tableName == "users" && from == "inviter_user_id" {
			t.Fatalf("room_invites.inviter_user_id must not cascade with users, on_delete=%s", onDelete)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate room_invites foreign keys: %v", err)
	}
}

func findMessage(t *testing.T, response map[string]any, messageID string) map[string]any {
	t.Helper()
	items, ok := response["messages"].([]any)
	if !ok {
		t.Fatalf("messages response missing messages: %v", response)
	}
	for _, item := range items {
		msg, ok := item.(map[string]any)
		if ok && msg["id"] == messageID {
			return msg
		}
	}
	t.Fatalf("message %s not found in response: %v", messageID, response)
	return nil
}

func roomCardByID(t *testing.T, response map[string]any, roomID string) map[string]any {
	t.Helper()
	items, ok := response["rooms"].([]any)
	if !ok {
		t.Fatalf("rooms response missing rooms: %v", response)
	}
	for _, item := range items {
		room, ok := item.(map[string]any)
		if ok && room["id"] == roomID {
			return room
		}
	}
	t.Fatalf("room %s not found in response: %v", roomID, response)
	return nil
}

func listRoomMessages(t *testing.T, api *apiHarness, token, roomID string) []map[string]any {
	t.Helper()
	status, response := api.request(http.MethodGet, "/rooms/"+roomID+"/messages?limit=100", token, nil)
	api.requireStatus(status, http.StatusOK, response)
	items, ok := response["messages"].([]any)
	if !ok {
		t.Fatalf("messages response missing messages: %v", response)
	}
	messages := make([]map[string]any, 0, len(items))
	for _, item := range items {
		msg, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("message item is not an object: %v", item)
		}
		messages = append(messages, msg)
	}
	return messages
}

func requireSystemMessage(t *testing.T, messages []map[string]any, event, subjectID string) map[string]any {
	t.Helper()
	for _, msg := range messages {
		if msg["type"] != systemMessageType {
			continue
		}
		attachment := systemAttachment(t, msg)
		if attachment["event"] != event {
			continue
		}
		if systemAttachmentSubjectID(t, attachment) == subjectID {
			return msg
		}
	}
	t.Fatalf("system message %s for %s not found: %v", event, subjectID, messages)
	return nil
}

func hasSystemMessage(t *testing.T, messages []map[string]any, event, subjectID string) bool {
	t.Helper()
	for _, msg := range messages {
		if msg["type"] != systemMessageType {
			continue
		}
		attachment := systemAttachment(t, msg)
		if attachment["event"] == event && systemAttachmentSubjectID(t, attachment) == subjectID {
			return true
		}
	}
	return false
}

func systemAttachment(t *testing.T, msg map[string]any) map[string]any {
	t.Helper()
	items, ok := msg["attachments"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("system message missing attachments: %v", msg)
	}
	attachment, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("system attachment is not an object: %v", items[0])
	}
	return attachment
}

func systemAttachmentSubjectID(t *testing.T, attachment map[string]any) string {
	t.Helper()
	if target, ok := attachment["target"].(map[string]any); ok {
		return target["id"].(string)
	}
	user, ok := attachment["user"].(map[string]any)
	if !ok {
		t.Fatalf("system attachment missing user: %v", attachment)
	}
	return user["id"].(string)
}

func requireSystemRoleChange(t *testing.T, messages []map[string]any, subjectID, toRole string) map[string]any {
	t.Helper()
	for _, msg := range messages {
		if msg["type"] != systemMessageType {
			continue
		}
		attachment := systemAttachment(t, msg)
		if attachment["event"] == systemEventRoomRoleChanged &&
			systemAttachmentSubjectID(t, attachment) == subjectID &&
			attachment["to_role"] == toRole {
			return msg
		}
	}
	t.Fatalf("role-change system message for %s to %s not found: %v", subjectID, toRole, messages)
	return nil
}

func TestPublicUIDAndRIDRanges(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_ids")
	other := api.register("other_ids")

	ownerUID := parseNumericID(t, owner.User["uid"])
	otherUID := parseNumericID(t, other.User["uid"])
	if ownerUID < idgen.UserUIDStart || otherUID <= ownerUID {
		t.Fatalf("unexpected uid sequence: owner=%d other=%d", ownerUID, otherUID)
	}

	room := api.createRoom(owner.Token, map[string]any{"name": "ID Range", "join_policy": "open"})
	rid := parseNumericID(t, room["rid"])
	if rid < idgen.RoomRIDStart {
		t.Fatalf("rid below configured range: %d", rid)
	}
}

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

func TestListRoomsOrdersLiveFirstThenLatestMessage(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("room_order_owner")
	liveRoom := api.createRoom(owner.Token, map[string]any{"name": "Live Old", "join_policy": "open"})
	oldRoom := api.createRoom(owner.Token, map[string]any{"name": "Old", "join_policy": "open"})
	newRoom := api.createRoom(owner.Token, map[string]any{"name": "New", "join_policy": "open"})
	liveRoomID := liveRoom["id"].(string)
	oldRoomID := oldRoom["id"].(string)
	newRoomID := newRoom["id"].(string)

	base := nowMillis()
	liveMessage := api.sendMessage(owner.Token, liveRoomID, "live but older")
	oldMessage := api.sendMessage(owner.Token, oldRoomID, "older")
	newMessage := api.sendMessage(owner.Token, newRoomID, "newer")
	setMessageCreatedAt := func(message map[string]any, createdAt int64) {
		t.Helper()
		if _, err := api.db.Exec(`UPDATE messages SET created_at = ? WHERE id = ?`, createdAt, message["id"].(string)); err != nil {
			t.Fatalf("set message created_at: %v", err)
		}
	}
	setMessageCreatedAt(liveMessage, base+1000)
	setMessageCreatedAt(oldMessage, base+2000)
	setMessageCreatedAt(newMessage, base+3000)

	status, response := api.request(http.MethodPost, "/rooms/"+liveRoomID+"/live/join", owner.Token, map[string]any{
		"client_live_session_id": "clive_order_owner",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/rooms?limit=10", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	rooms, ok := response["rooms"].([]any)
	if !ok || len(rooms) < 3 {
		t.Fatalf("rooms response missing entries: %v", response)
	}
	got := []string{
		rooms[0].(map[string]any)["id"].(string),
		rooms[1].(map[string]any)["id"].(string),
		rooms[2].(map[string]any)["id"].(string),
	}
	want := []string{liveRoomID, newRoomID, oldRoomID}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("room order mismatch: got %v want first three %v", got, want)
		}
	}
}

func TestAttachmentDispositionUsesUTF8FilenameStar(t *testing.T) {
	header := attachmentDisposition("表情 包.webp")
	if strings.Contains(header, "表情") {
		t.Fatalf("ASCII fallback filename should not contain raw Chinese: %q", header)
	}
	if !strings.Contains(header, `filename="__ _.webp"`) {
		t.Fatalf("header should include ASCII fallback filename: %q", header)
	}
	if !strings.Contains(header, `filename*=UTF-8''%E8%A1%A8%E6%83%85%20%E5%8C%85.webp`) {
		t.Fatalf("header should include RFC 5987 UTF-8 filename: %q", header)
	}
}

func TestSuperuserCanSeeAndJoinPrivateRooms(t *testing.T) {
	api := newAPIHarness(t)
	super := api.login("GANG", "64n9-Ch47")
	if super.User["uid"] != idgen.ReservedSuperUID || super.User["is_superuser"] != true {
		t.Fatalf("reserved superuser fields not returned: %v", super.User)
	}

	owner := api.register("private_owner")
	outsider := api.register("private_outsider")
	room := api.createRoom(owner.Token, map[string]any{
		"name":        "Hidden Ops",
		"visibility":  "private",
		"join_policy": "closed",
	})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodGet, "/rooms", super.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	rooms := response["rooms"].([]any)
	if len(rooms) != 1 || rooms[0].(map[string]any)["id"] != roomID {
		t.Fatalf("superuser should list private room: %v", response)
	}

	status, response = api.request(http.MethodGet, "/rooms/search?q=Hidden&limit=20", outsider.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["rooms"].([]any)); got != 0 {
		t.Fatalf("normal user should not find private room by name, got %d: %v", got, response)
	}

	status, response = api.request(http.MethodGet, "/rooms/search?q=Hidden&limit=20", super.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["rooms"].([]any)); got != 1 {
		t.Fatalf("superuser should find private room by name, got %d: %v", got, response)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID, super.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	detail := response["room"].(map[string]any)
	membership := detail["my_membership"].(map[string]any)
	if membership["role"] != "superuser" {
		t.Fatalf("superuser should receive effective room role before joining: %v", membership)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", super.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	joined := response["room"].(map[string]any)["my_membership"].(map[string]any)
	if joined["role"] != "superuser" {
		t.Fatalf("superuser should keep an effective temporary room role: %v", joined)
	}
	var superMemberships int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, super.User["id"].(string)).Scan(&superMemberships); err != nil {
		t.Fatalf("count superuser memberships: %v", err)
	}
	if superMemberships != 0 {
		t.Fatalf("superuser should not be persisted as a room member, got %d rows", superMemberships)
	}

	status, response = api.request(http.MethodDelete, "/rooms/"+roomID+"/members/me", super.Token, nil)
	api.requireStatus(status, http.StatusForbidden, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/leave", super.Token, nil)
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(http.MethodDelete, "/rooms/"+roomID+"/members/"+super.User["id"].(string), owner.Token, nil)
	api.requireStatus(status, http.StatusForbidden, response)
}

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
	}

	status, response = api.request(http.MethodGet, "/search?q=needle-paging&limit=1&categories=unknown,,", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	for _, category := range []string{"my_rooms", "public_rooms", "messages", "files"} {
		items, ok := response[category].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("search with no valid categories should fall back to all categories; %s got %v in %v", category, response[category], response)
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
}

func TestMemberProfileIncludesBioAndRoomLinks(t *testing.T) {
	api := newAPIHarness(t)
	super := api.login("GANG", "64n9-Ch47")
	owner := api.register("profile_owner")
	alice := api.register("profile_alice")
	viewer := api.register("profile_viewer")

	room1 := api.createRoom(owner.Token, map[string]any{"name": "Shared", "join_policy": "open"})
	room1ID := room1["id"].(string)
	room2 := api.createRoom(owner.Token, map[string]any{"name": "Alice Only", "join_policy": "open"})
	room2ID := room2["id"].(string)
	status, response := api.request(http.MethodPost, "/rooms/"+room1ID+"/join", alice.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+room1ID+"/join", viewer.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+room2ID+"/join", alice.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	if _, err := api.db.Exec(`UPDATE users SET bio = ? WHERE id = ?`, "Ships quietly", alice.User["id"].(string)); err != nil {
		t.Fatalf("update alice bio: %v", err)
	}
	if _, err := api.db.Exec(
		`UPDATE room_memberships SET remark_name = ?, room_display_name = ? WHERE room_id = ? AND user_id = ?`,
		"Shared Remark", "Alice Shared", room1ID, alice.User["id"].(string),
	); err != nil {
		t.Fatalf("update room remark: %v", err)
	}
	aliceSub := api.bus.Subscribe(alice.User["id"].(string))
	defer aliceSub.Close()

	status, response = api.request(http.MethodGet, "/rooms/"+room1ID+"/members/"+alice.User["id"].(string)+"/profile", viewer.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	profile := response["profile"].(map[string]any)
	user := profile["user"].(map[string]any)
	if user["bio"] != "Ships quietly" {
		t.Fatalf("profile should include signature: %v", profile)
	}
	if user["is_online"] != true {
		t.Fatalf("profile should include online state: %v", profile)
	}
	commonRooms := user["common_rooms"].([]any)
	if len(commonRooms) != 1 {
		t.Fatalf("viewer should see only common rooms: %v", commonRooms)
	}
	commonRoom := commonRooms[0].(map[string]any)
	if commonRoom["id"] != room1ID || commonRoom["remark_name"] != "Shared Remark" || commonRoom["rid"] == "" {
		t.Fatalf("common room should include id rid and remark: %v", commonRoom)
	}
	if commonRoom["room_display_name"] != "Alice Shared" || commonRoom["room_role"] != "member" {
		t.Fatalf("common room should include target room identity: %v", commonRoom)
	}

	status, response = api.request(http.MethodGet, "/users/"+alice.User["id"].(string)+"/profile", viewer.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	user = response["profile"].(map[string]any)["user"].(map[string]any)
	if user["bio"] != "Ships quietly" || user["is_online"] != true {
		t.Fatalf("global profile should include latest user state: %v", user)
	}
	commonRooms = user["common_rooms"].([]any)
	if len(commonRooms) != 1 || commonRooms[0].(map[string]any)["id"] != room1ID {
		t.Fatalf("global profile should include viewer-visible common rooms: %v", commonRooms)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+room1ID+"/members/"+alice.User["id"].(string)+"/profile", super.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	user = response["profile"].(map[string]any)["user"].(map[string]any)
	if got := len(user["common_rooms"].([]any)); got != 2 {
		t.Fatalf("superuser should see all target rooms, got %d: %v", got, user["common_rooms"])
	}

	status, response = api.request(http.MethodGet, "/rooms/"+room1ID+"/members/"+super.User["id"].(string)+"/profile", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	superProfile := response["profile"].(map[string]any)
	superUser := superProfile["user"].(map[string]any)
	if superProfile["role"] != "superuser" || superUser["is_superuser"] != true {
		t.Fatalf("superuser ghost profile should be visible with role: %v", superProfile)
	}
	if _, ok := superUser["common_rooms"]; ok {
		t.Fatalf("superuser profile should omit common_rooms: %v", superUser)
	}
}

func TestRoomOnlineMemberCountUsesActiveConnections(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("online_owner")
	alice := api.register("online_alice")
	room := api.createRoom(owner.Token, map[string]any{"name": "Presence", "join_policy": "open"})
	roomID := room["id"].(string)
	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", alice.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/members?limit=50", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	ownerMember := memberByUserID(t, response, owner.User["id"].(string))
	aliceMember := memberByUserID(t, response, alice.User["id"].(string))
	if ownerMember["user"].(map[string]any)["is_online"] != true {
		t.Fatalf("current requester should be online even before SSE connects: %v", response)
	}
	if aliceMember["user"].(map[string]any)["is_online"] != false {
		t.Fatalf("other members should still depend on active connections: %v", response)
	}

	ownerA := api.bus.Subscribe(owner.User["id"].(string))
	defer ownerA.Close()
	ownerB := api.bus.Subscribe(owner.User["id"].(string))
	defer ownerB.Close()
	aliceSub := api.bus.Subscribe(alice.User["id"].(string))
	defer aliceSub.Close()
	outsiderSub := api.bus.Subscribe("not_a_member")
	defer outsiderSub.Close()

	status, response = api.request(http.MethodGet, "/rooms/"+roomID, owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	detail := response["room"].(map[string]any)
	if detail["member_count"] != float64(2) || detail["online_member_count"] != float64(2) {
		t.Fatalf("room detail should count unique online members only: %v", detail)
	}

	status, response = api.request(http.MethodGet, "/rooms", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	card := response["rooms"].([]any)[0].(map[string]any)
	if card["online_member_count"] != float64(2) {
		t.Fatalf("room card should include online member count: %v", card)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/members?limit=50", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	ownerMember = memberByUserID(t, response, owner.User["id"].(string))
	aliceMember = memberByUserID(t, response, alice.User["id"].(string))
	if ownerMember["user"].(map[string]any)["is_online"] != true ||
		aliceMember["user"].(map[string]any)["is_online"] != true {
		t.Fatalf("member payload should expose online state: %v", response)
	}
}

func TestRoomLeaveDeletesAndPromotesAdmin(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("leave_owner")
	alice := api.register("leave_alice")
	bob := api.register("leave_bob")

	room := api.createRoom(owner.Token, map[string]any{"name": "Leave Flow", "join_policy": "open"})
	roomID := room["id"].(string)
	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", alice.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", bob.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/leave", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/members?limit=50", alice.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	aliceMember := memberByUserID(t, response, alice.User["id"].(string))
	bobMember := memberByUserID(t, response, bob.User["id"].(string))
	ownerCount := 0
	for _, member := range []map[string]any{aliceMember, bobMember} {
		if member["role"] == "owner" {
			ownerCount++
		}
	}
	if ownerCount != 1 {
		t.Fatalf("exactly one remaining member should be promoted to owner: %v", response)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	rejoined := response["room"].(map[string]any)["my_membership"].(map[string]any)
	if rejoined["role"] != "member" {
		t.Fatalf("departed admin should rejoin as member: %v", rejoined)
	}

	solo := api.createRoom(owner.Token, map[string]any{"name": "Solo", "join_policy": "open"})
	soloID := solo["id"].(string)
	// Owner is the only member, so leaving deletes the room — which now
	// requires explicit confirmation. Without it the server returns 409.
	status, response = api.request(http.MethodPost, "/rooms/"+soloID+"/leave", owner.Token, nil)
	api.requireStatus(status, http.StatusConflict, response)
	status, response = api.request(http.MethodPost, "/rooms/"+soloID+"/leave", owner.Token, map[string]any{"confirm_delete_if_empty": true})
	api.requireStatus(status, http.StatusOK, response)
	var exists int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM rooms WHERE id = ?`, soloID).Scan(&exists); err != nil {
		t.Fatalf("query solo room: %v", err)
	}
	if exists != 0 {
		t.Fatalf("room should be deleted after last member leaves")
	}
}

func TestRoomInfoManagementEndpoints(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("manage_owner")
	member := api.register("manage_member")

	room := api.createRoom(owner.Token, map[string]any{
		"name":                           "Manage Me",
		"description":                    "old intro",
		"join_policy":                    "open",
		"ai_voice_announcements_enabled": false,
		"default_avatar_key":             "green-2",
	})
	if room["description"] != "old intro" || room["ai_voice_announcements_enabled"] != false || room["default_avatar_key"] != "green-2" {
		t.Fatalf("create room response missing management fields: %v", room)
	}
	roomID := room["id"].(string)
	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID, owner.Token, map[string]any{
		"name":                           "Managed",
		"description":                    "Room bio",
		"visibility":                     "private",
		"join_policy":                    "closed",
		"ai_voice_announcements_enabled": false,
		"default_avatar_key":             "green-2",
	})
	api.requireStatus(status, http.StatusOK, response)
	managed := response["room"].(map[string]any)
	if managed["name"] != "Managed" || managed["description"] != "Room bio" || managed["visibility"] != "private" || managed["join_policy"] != "closed" {
		t.Fatalf("room management response missing updated fields: %v", managed)
	}
	if managed["ai_voice_announcements_enabled"] != false || managed["default_avatar_key"] != "green-2" {
		t.Fatalf("room management response missing voice/avatar fields: %v", managed)
	}
	createdBy := managed["created_by"].(map[string]any)
	if createdBy["id"] != owner.User["id"] {
		t.Fatalf("room creator should be owner before transfer: %v", managed)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/me", member.Token, map[string]any{
		"remark_name":         "My Managed Room",
		"room_display_name":   "Local Nick",
		"default_avatar_key":  "red-4",
		"notification_policy": "mentions",
	})
	api.requireStatus(status, http.StatusOK, response)
	personalRoom := response["room"].(map[string]any)
	profile := personalRoom["personal_profile"].(map[string]any)
	if personalRoom["remark_name"] != "My Managed Room" || personalRoom["notification_policy"] != "mentions" {
		t.Fatalf("room personal fields not returned on detail: %v", personalRoom)
	}
	if profile["display_name"] != "Local Nick" || profile["default_avatar_key"] != "red-4" {
		t.Fatalf("room personal profile not returned: %v", profile)
	}
	settings := response["settings"].(map[string]any)
	if settings["notification_policy"] != "mentions" {
		t.Fatalf("settings should expose notification_policy alias: %v", settings)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+member.User["id"].(string), member.Token, map[string]any{"role": "admin"})
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+member.User["id"].(string), owner.Token, map[string]any{"role": "admin"})
	api.requireStatus(status, http.StatusOK, response)
	if response["member"].(map[string]any)["role"] != "admin" {
		t.Fatalf("owner should grant admin role: %v", response)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/creator", owner.Token, map[string]any{
		"user_id": member.User["id"].(string),
	})
	api.requireStatus(status, http.StatusOK, response)
	transferred := response["room"].(map[string]any)
	if transferred["created_by"].(map[string]any)["id"] != member.User["id"] {
		t.Fatalf("creator transfer should update room creator: %v", transferred)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/members?limit=50", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if memberByUserID(t, response, member.User["id"].(string))["role"] != "owner" {
		t.Fatalf("target member should become owner: %v", response)
	}
	if memberByUserID(t, response, owner.User["id"].(string))["role"] != "admin" {
		t.Fatalf("previous owner should become admin: %v", response)
	}

	status, response = api.request(http.MethodDelete, "/rooms/"+roomID+"/members/me", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/rooms/"+roomID, owner.Token, nil)
	api.requireStatus(status, http.StatusNotFound, response)

	status, response = api.request(http.MethodDelete, "/rooms/"+roomID, member.Token, map[string]any{"confirm_name": "Managed"})
	api.requireStatus(status, http.StatusOK, response)
	var exists int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM rooms WHERE id = ?`, roomID).Scan(&exists); err != nil {
		t.Fatalf("count deleted room: %v", err)
	}
	if exists != 0 {
		t.Fatalf("room should be deleted after confirmed creator deletion")
	}
}

func TestOnlyAdminsCanRemoveMembers(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("remove_owner")
	admin := api.register("remove_admin")
	peerAdmin := api.register("remove_peer_admin")
	regular := api.register("remove_regular")
	target := api.register("remove_target")

	room := api.createRoom(owner.Token, map[string]any{"name": "Remove Gate", "join_policy": "open"})
	roomID := room["id"].(string)
	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", admin.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", regular.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", peerAdmin.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", target.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+admin.User["id"].(string), owner.Token, map[string]any{"role": "admin"})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+peerAdmin.User["id"].(string), owner.Token, map[string]any{"role": "admin"})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodDelete, "/rooms/"+roomID+"/members/"+target.User["id"].(string), regular.Token, nil)
	api.requireStatus(status, http.StatusForbidden, response)

	var targetMemberships int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, target.User["id"].(string)).Scan(&targetMemberships); err != nil {
		t.Fatalf("count target memberships after regular remove attempt: %v", err)
	}
	if targetMemberships != 1 {
		t.Fatalf("regular member should not remove target membership, got %d", targetMemberships)
	}

	status, response = api.request(http.MethodDelete, "/rooms/"+roomID+"/members/"+peerAdmin.User["id"].(string), admin.Token, nil)
	api.requireStatus(status, http.StatusForbidden, response)
	var peerAdminMemberships int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, peerAdmin.User["id"].(string)).Scan(&peerAdminMemberships); err != nil {
		t.Fatalf("count peer admin memberships after admin remove attempt: %v", err)
	}
	if peerAdminMemberships != 1 {
		t.Fatalf("admin should not remove peer admin membership, got %d", peerAdminMemberships)
	}

	status, response = api.request(http.MethodDelete, "/rooms/"+roomID+"/members/"+target.User["id"].(string), admin.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, target.User["id"].(string)).Scan(&targetMemberships); err != nil {
		t.Fatalf("count target memberships after admin remove: %v", err)
	}
	if targetMemberships != 0 {
		t.Fatalf("admin should remove target membership, got %d", targetMemberships)
	}
}

func TestSystemMessagesForRoomEvents(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("system_owner")
	member := api.register("system_member")
	target := api.register("system_target")
	room := api.createRoom(owner.Token, map[string]any{"name": "System Events", "join_policy": "open"})
	roomID := room["id"].(string)
	ownerID := owner.User["id"].(string)
	memberID := member.User["id"].(string)
	targetID := target.User["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", target.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/join", member.Token, map[string]any{
		"client_live_session_id": "system_live_member",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me", member.Token, map[string]any{
		"connection_state": "left",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+memberID, owner.Token, map[string]any{"role": "admin"})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/creator", owner.Token, map[string]any{"user_id": memberID})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodDelete, "/rooms/"+roomID+"/members/"+targetID, owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	messages := listRoomMessages(t, api, member.Token, roomID)
	join := requireSystemMessage(t, messages, systemEventRoomMemberJoined, memberID)
	if join["body"] != "加入了房间" {
		t.Fatalf("join message should carry sidebar body: %v", join)
	}
	if hasSystemMessage(t, messages, systemEventLiveJoined, memberID) {
		t.Fatalf("live join should not create a system message: %v", messages)
	}
	if hasSystemMessage(t, messages, systemEventLiveLeft, memberID) {
		t.Fatalf("live leave should not create a system message: %v", messages)
	}

	admin := requireSystemRoleChange(t, messages, memberID, "admin")
	adminAttachment := systemAttachment(t, admin)
	if adminAttachment["from_role"] != "member" {
		t.Fatalf("admin role system attachment mismatch: %v", adminAttachment)
	}
	if actor := adminAttachment["actor"].(map[string]any); actor["id"] != ownerID {
		t.Fatalf("admin role system message should include actor: %v", adminAttachment)
	}

	ownerTransfer := requireSystemRoleChange(t, messages, memberID, "owner")
	if ownerTransfer["body"] == "" {
		t.Fatalf("creator promotion should carry a body: %v", ownerTransfer)
	}
	ownerDemotion := requireSystemRoleChange(t, messages, ownerID, "admin")
	ownerDemotionAttachment := systemAttachment(t, ownerDemotion)
	if ownerDemotionAttachment["from_role"] != "owner" {
		t.Fatalf("owner demotion attachment mismatch: %v", ownerDemotionAttachment)
	}
	if ownerDemotion["body"] != "降职为管理员" {
		t.Fatalf("owner demotion body should omit actor: %v", ownerDemotion)
	}

	removed := requireSystemMessage(t, messages, systemEventRoomMemberRemoved, targetID)
	removedAttachment := systemAttachment(t, removed)
	if actor := removedAttachment["actor"].(map[string]any); actor["id"] != ownerID {
		t.Fatalf("removed system message should include actor: %v", removedAttachment)
	}
}

func TestSuperuserTransferCreatorDemotesPreviousOwner(t *testing.T) {
	api := newAPIHarness(t)
	super := api.login("GANG", "64n9-Ch47")
	owner := api.register("super_transfer_owner")
	member := api.register("super_transfer_member")

	room := api.createRoom(owner.Token, map[string]any{"name": "Super Transfer", "join_policy": "open"})
	roomID := room["id"].(string)
	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/creator", super.Token, map[string]any{
		"user_id": member.User["id"].(string),
	})
	api.requireStatus(status, http.StatusOK, response)
	transferred := response["room"].(map[string]any)
	if transferred["created_by"].(map[string]any)["id"] != member.User["id"] {
		t.Fatalf("superuser creator transfer should update room creator: %v", transferred)
	}
	if transferred["my_membership"].(map[string]any)["role"] != "superuser" {
		t.Fatalf("superuser should remain a temporary superuser role: %v", transferred)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/members?limit=50", super.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if memberByUserID(t, response, member.User["id"].(string))["role"] != "owner" {
		t.Fatalf("target member should become owner: %v", response)
	}
	if memberByUserID(t, response, owner.User["id"].(string))["role"] != "admin" {
		t.Fatalf("previous owner should become admin: %v", response)
	}
	var superMemberships int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, super.User["id"].(string)).Scan(&superMemberships); err != nil {
		t.Fatalf("count superuser memberships: %v", err)
	}
	if superMemberships != 0 {
		t.Fatalf("superuser should not become a room member during creator transfer")
	}
}

func TestSuperuserCreatedRoomFirstJoinerBecomesOwner(t *testing.T) {
	api := newAPIHarness(t)
	super := api.login("GANG", "64n9-Ch47")
	joiner := api.register("seed_joiner")
	reviewed := api.register("seed_reviewed")
	invited := api.register("seed_invited")

	room := api.createRoom(super.Token, map[string]any{
		"name":        "Ghost Seed",
		"join_policy": "open",
	})
	roomID := room["id"].(string)
	if room["created_by"] != nil {
		t.Fatalf("superuser-created room should not expose a creator before first normal member joins: %v", room)
	}
	if room["my_membership"].(map[string]any)["role"] != "superuser" || int(room["member_count"].(float64)) != 0 {
		t.Fatalf("superuser-created room should return temporary role and no members: %v", room)
	}
	var superMemberships int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, super.User["id"].(string)).Scan(&superMemberships); err != nil {
		t.Fatalf("count superuser memberships: %v", err)
	}
	if superMemberships != 0 {
		t.Fatalf("superuser should not be inserted as room member")
	}

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", joiner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	joinedRoom := response["room"].(map[string]any)
	if joinedRoom["my_membership"].(map[string]any)["role"] != "owner" {
		t.Fatalf("first normal joiner should become owner: %v", joinedRoom)
	}
	if joinedRoom["created_by"].(map[string]any)["id"] != joiner.User["id"] {
		t.Fatalf("first normal joiner should become visible creator: %v", joinedRoom)
	}

	reviewRoom := api.createRoom(super.Token, map[string]any{"name": "Review Seed"})
	reviewRoomID := reviewRoom["id"].(string)
	status, response = api.request(http.MethodPost, "/rooms/"+reviewRoomID+"/join", reviewed.Token, nil)
	api.requireStatus(status, http.StatusAccepted, response)
	requestID := response["join_request"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodPatch, "/rooms/"+reviewRoomID+"/join-requests/"+requestID, super.Token, map[string]any{"decision": "approve"})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/rooms/"+reviewRoomID, reviewed.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	reviewedRoom := response["room"].(map[string]any)
	if reviewedRoom["my_membership"].(map[string]any)["role"] != "owner" || reviewedRoom["created_by"].(map[string]any)["id"] != reviewed.User["id"] {
		t.Fatalf("approved first normal joiner should become owner: %v", reviewedRoom)
	}

	inviteRoom := api.createRoom(super.Token, map[string]any{"name": "Invite Seed", "join_policy": "approval_required"})
	inviteRoomID := inviteRoom["id"].(string)
	status, response = api.request(http.MethodPost, "/rooms/"+inviteRoomID+"/invites", super.Token, map[string]any{
		"user_id": invited.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	inviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodPatch, "/room-invites/"+inviteID, invited.Token, map[string]any{"decision": "accept"})
	api.requireStatus(status, http.StatusOK, response)
	invitedRoom := response["room"].(map[string]any)
	if invitedRoom["my_membership"].(map[string]any)["role"] != "owner" || invitedRoom["created_by"].(map[string]any)["id"] != invited.User["id"] {
		t.Fatalf("invited first normal member should become owner: %v", invitedRoom)
	}
}

func TestLiveHeadphonesAndVoiceBlock(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("voice_owner")
	member := api.register("voice_member")
	room := api.createRoom(owner.Token, map[string]any{"name": "Voice Room", "join_policy": "open"})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/join", member.Token, map[string]any{
		"client_live_session_id": "clive_test_member",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me", member.Token, map[string]any{
		"headphones_muted": true,
	})
	api.requireStatus(status, http.StatusOK, response)
	participant := response["participant"].(map[string]any)
	if participant["headphones_muted"] != true {
		t.Fatalf("headphones should be muted: %v", participant)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/participants/"+member.User["id"].(string)+"/moderation", owner.Token, map[string]any{
		"action": "block_voice",
		"reason": "test",
	})
	api.requireStatus(status, http.StatusOK, response)

	// block_voice must drive the LiveKit media session, not just the DB: revoke
	// publish and server-side mute the mic.
	memberKey := roomID + "/" + member.User["id"].(string)
	if !contains(api.live.publishSet, memberKey+"=false") {
		t.Fatalf("block_voice should revoke publish on LiveKit: %v", api.live.publishSet)
	}
	if !contains(api.live.micMuted, memberKey+"=true") {
		t.Fatalf("block_voice should server-mute the mic on LiveKit: %v", api.live.micMuted)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/live", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	live := response["live"].(map[string]any)
	participant = participantByUserID(t, live, member.User["id"].(string))
	if participant["mic_muted"] != true || participant["headphones_muted"] != true || participant["voice_blocked"] != true {
		t.Fatalf("voice block should force mic and headphones off: %v", participant)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me", member.Token, map[string]any{
		"mic_muted":        false,
		"headphones_muted": false,
	})
	api.requireStatus(status, http.StatusOK, response)
	participant = response["participant"].(map[string]any)
	if participant["mic_muted"] != true || participant["headphones_muted"] != true || participant["voice_blocked"] != true {
		t.Fatalf("voice blocked user should not be able to unmute: %v", participant)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/participants/"+member.User["id"].(string)+"/moderation", owner.Token, map[string]any{
		"action": "restore_voice",
	})
	api.requireStatus(status, http.StatusOK, response)
	if !contains(api.live.publishSet, memberKey+"=true") {
		t.Fatalf("restore_voice should re-grant publish on LiveKit: %v", api.live.publishSet)
	}
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me", member.Token, map[string]any{
		"headphones_muted": false,
	})
	api.requireStatus(status, http.StatusOK, response)
	participant = response["participant"].(map[string]any)
	if participant["voice_blocked"] != false || participant["headphones_muted"] != false {
		t.Fatalf("voice restore should allow headphones again: %v", participant)
	}

	// A voice ban must outlive the live session: re-block, leave (drop the
	// participant row), rejoin, and confirm the user comes back blocked.
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/participants/"+member.User["id"].(string)+"/moderation", owner.Token, map[string]any{
		"action": "block_voice",
	})
	api.requireStatus(status, http.StatusOK, response)
	// Simulate a disconnect: clear the live row the way leave / the webhook would.
	if _, err := api.db.Exec(`DELETE FROM live_participants WHERE room_id = ? AND user_id = ?`, roomID, member.User["id"].(string)); err != nil {
		t.Fatalf("failed to clear live participant: %v", err)
	}
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/join", member.Token, map[string]any{
		"client_live_session_id": "clive_test_member_rejoin",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)
	participant = response["participant"].(map[string]any)
	if participant["voice_blocked"] != true || participant["mic_muted"] != true {
		t.Fatalf("voice ban should persist across rejoin: %v", participant)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestUserAudioSettings(t *testing.T) {
	api := newAPIHarness(t)
	user := api.register("audio_settings_user")

	status, response := api.request(http.MethodGet, "/users/me/audio-settings", user.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	settings := response["audio_settings"].(map[string]any)
	if settings["default_audio_input_volume"] != float64(100) ||
		settings["default_audio_output_volume"] != float64(100) ||
		settings["live_mic_input_volume"] != float64(100) ||
		settings["live_voice_output_volume"] != float64(100) ||
		settings["live_screen_share_output_volume"] != float64(100) ||
		settings["live_music_output_volume"] != float64(100) {
		t.Fatalf("default audio settings should be 100 percent: %v", settings)
	}

	status, response = api.request(http.MethodPatch, "/users/me/audio-settings", user.Token, map[string]any{
		"default_audio_input_volume":      72,
		"live_voice_output_volume":        48,
		"live_screen_share_output_volume": 35,
		"live_music_output_volume":        0,
	})
	api.requireStatus(status, http.StatusOK, response)
	settings = response["audio_settings"].(map[string]any)
	if settings["default_audio_input_volume"] != float64(72) ||
		settings["default_audio_output_volume"] != float64(100) ||
		settings["live_voice_output_volume"] != float64(48) ||
		settings["live_screen_share_output_volume"] != float64(35) ||
		settings["live_music_output_volume"] != float64(0) {
		t.Fatalf("patched audio settings not persisted in response: %v", settings)
	}

	status, response = api.request(http.MethodGet, "/users/me/audio-settings", user.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	settings = response["audio_settings"].(map[string]any)
	if settings["default_audio_input_volume"] != float64(72) || settings["live_music_output_volume"] != float64(0) {
		t.Fatalf("patched audio settings should persist: %v", settings)
	}

	status, response = api.request(http.MethodPatch, "/users/me/audio-settings", user.Token, map[string]any{
		"live_music_output_volume": 101,
	})
	api.requireStatus(status, http.StatusBadRequest, response)

	status, response = api.request(http.MethodPatch, "/users/me/audio-settings", user.Token, map[string]any{})
	api.requireStatus(status, http.StatusBadRequest, response)
}

func TestAccountLanguagePreference(t *testing.T) {
	api := newAPIHarness(t)
	user := api.register("language_user")
	if user.User["language"] != "zh-Hans" {
		t.Fatalf("new users should default to Simplified Chinese: %v", user.User)
	}

	status, response := api.request(http.MethodPatch, "/users/me/account", user.Token, map[string]any{
		"language": "zh-Hant",
	})
	api.requireStatus(status, http.StatusOK, response)
	updated := response["user"].(map[string]any)
	if updated["language"] != "zh-Hant" {
		t.Fatalf("language preference should update in account response: %v", updated)
	}

	status, response = api.request(http.MethodGet, "/me", user.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if response["language"] != "zh-Hant" {
		t.Fatalf("language preference should persist on /me: %v", response)
	}

	status, response = api.request(http.MethodPatch, "/users/me/account", user.Token, map[string]any{
		"language": "fr",
	})
	api.requireStatus(status, http.StatusBadRequest, response)
}

func TestLiveMemberVolumes(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("volume_owner")
	member := api.register("volume_member")
	outsider := api.register("volume_outsider")
	room := api.createRoom(owner.Token, map[string]any{"name": "Member Volume Room", "join_policy": "open"})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/live/me/member-volumes", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if volumes := response["member_volumes"].([]any); len(volumes) != 0 {
		t.Fatalf("member volumes should start empty: %v", response)
	}

	targetID := member.User["id"].(string)
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me/member-volumes/"+targetID, owner.Token, map[string]any{
		"volume": 37,
	})
	api.requireStatus(status, http.StatusOK, response)
	volume := response["member_volume"].(map[string]any)
	target := volume["target_user"].(map[string]any)
	if volume["volume"] != float64(37) || target["id"] != targetID {
		t.Fatalf("member volume response mismatch: %v", volume)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/live/me/member-volumes", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	volumes := response["member_volumes"].([]any)
	if len(volumes) != 1 || volumes[0].(map[string]any)["volume"] != float64(37) {
		t.Fatalf("member volume should be listed for listener: %v", response)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/live/me/member-volumes", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if volumes := response["member_volumes"].([]any); len(volumes) != 0 {
		t.Fatalf("member volume should be scoped to the listener: %v", response)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me/member-volumes/"+targetID, owner.Token, map[string]any{
		"volume": 101,
	})
	api.requireStatus(status, http.StatusBadRequest, response)

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me/member-volumes/"+owner.User["id"].(string), owner.Token, map[string]any{
		"volume": 50,
	})
	api.requireStatus(status, http.StatusBadRequest, response)

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me/member-volumes/"+outsider.User["id"].(string), owner.Token, map[string]any{
		"volume": 50,
	})
	api.requireStatus(status, http.StatusNotFound, response)
}

func TestSaveStickerToPersonalAndRoomPacks(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("sticker_owner")
	member := api.register("sticker_member")
	room := api.createRoom(owner.Token, map[string]any{"name": "Sticker Room", "join_policy": "open"})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	assetID := "asset_test_sticker"
	_, err := api.db.Exec(
		`INSERT INTO assets (id, owner_user_id, purpose, filename, mime_type, size_bytes, url, created_at)
		 VALUES (?, ?, 'sticker', 'ok.webp', 'image/webp', 12, 'https://example.com/ok.webp', ?)`,
		assetID, owner.User["id"].(string), nowMillis(),
	)
	if err != nil {
		t.Fatalf("insert asset: %v", err)
	}

	status, response = api.request(http.MethodPost, "/sticker-packs", owner.Token, map[string]any{
		"scope":   "room",
		"room_id": roomID,
		"name":    "Room Source",
	})
	api.requireStatus(status, http.StatusCreated, response)
	sourcePack := response["pack"].(map[string]any)
	status, response = api.request(http.MethodPost, "/sticker-packs/"+sourcePack["id"].(string)+"/stickers", owner.Token, map[string]any{
		"asset_id": assetID,
		"name":     "ok",
	})
	api.requireStatus(status, http.StatusCreated, response)
	sourceSticker := response["sticker"].(map[string]any)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/stickers/save", member.Token, map[string]any{
		"sticker_id":   sourceSticker["id"].(string),
		"target_scope": "personal",
	})
	api.requireStatus(status, http.StatusCreated, response)
	personalPack := response["pack"].(map[string]any)
	if personalPack["scope"] != "personal" {
		t.Fatalf("member should save to a personal pack: %v", response)
	}
	if personalPack["name"] != defaultPersonalStickerPackName {
		t.Fatalf("member should save to the default personal pack: %v", response)
	}
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/stickers/save", member.Token, map[string]any{
		"sticker_id":   sourceSticker["id"].(string),
		"target_scope": "personal",
	})
	api.requireStatus(status, http.StatusCreated, response)
	if !hasStickerNames(response["pack"].(map[string]any), "ok", "ok (2)") {
		t.Fatalf("duplicate personal saved sticker should be suffixed: %v", response)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/stickers/save", member.Token, map[string]any{
		"sticker_id":   sourceSticker["id"].(string),
		"target_scope": "room",
	})
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/stickers/save", owner.Token, map[string]any{
		"sticker_id":   sourceSticker["id"].(string),
		"target_scope": "room",
	})
	api.requireStatus(status, http.StatusCreated, response)
	roomPack := response["pack"].(map[string]any)
	if roomPack["scope"] != "room" || roomPack["room_id"] != roomID {
		t.Fatalf("admin should save to room pack: %v", response)
	}
	if roomPack["name"] != defaultRoomStickerPackName {
		t.Fatalf("admin should save to the default room pack: %v", response)
	}
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/stickers/save", owner.Token, map[string]any{
		"sticker_id":   sourceSticker["id"].(string),
		"target_scope": "room",
	})
	api.requireStatus(status, http.StatusCreated, response)
	if !hasStickerNames(response["pack"].(map[string]any), "ok", "ok (2)") {
		t.Fatalf("duplicate room saved sticker should be suffixed: %v", response)
	}
}

func TestListStickerPacksRespectsScopeAndRoom(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("sticker_scope_owner")
	roomA := api.createRoom(owner.Token, map[string]any{"name": "Room A", "join_policy": "open"})
	roomB := api.createRoom(owner.Token, map[string]any{"name": "Room B", "join_policy": "open"})
	roomAID := roomA["id"].(string)
	roomBID := roomB["id"].(string)

	status, response := api.request(http.MethodPost, "/sticker-packs", owner.Token, map[string]any{
		"scope": "personal",
		"name":  "Mine",
	})
	api.requireStatus(status, http.StatusCreated, response)

	status, response = api.request(http.MethodPost, "/sticker-packs", owner.Token, map[string]any{
		"scope":   "room",
		"room_id": roomAID,
		"name":    "Room A Pack",
	})
	api.requireStatus(status, http.StatusCreated, response)
	roomAPackID := response["pack"].(map[string]any)["id"].(string)

	status, response = api.request(http.MethodPost, "/sticker-packs", owner.Token, map[string]any{
		"scope":   "room",
		"room_id": roomBID,
		"name":    "Room B Pack",
	})
	api.requireStatus(status, http.StatusCreated, response)

	status, response = api.request(http.MethodGet, "/sticker-packs?scope=room&room_id="+roomAID, owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	roomPacks := response["packs"].([]any)
	if len(roomPacks) != 1 {
		t.Fatalf("room list should include only that room's packs: %v", response)
	}
	roomPack := roomPacks[0].(map[string]any)
	if roomPack["id"] != roomAPackID || roomPack["scope"] != "room" || roomPack["room_id"] != roomAID {
		t.Fatalf("room list returned wrong pack: %v", roomPack)
	}

	status, response = api.request(http.MethodGet, "/sticker-packs?scope=personal", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	personalPacks := response["packs"].([]any)
	if len(personalPacks) != 1 || personalPacks[0].(map[string]any)["scope"] != "personal" {
		t.Fatalf("personal list should include only personal packs: %v", response)
	}
}

func TestAddStickerIsIdempotent(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("sticker_idempotent_owner")

	assetID := "asset_idempotent_sticker"
	_, err := api.db.Exec(
		`INSERT INTO assets (id, owner_user_id, purpose, filename, mime_type, size_bytes, url, created_at)
		 VALUES (?, ?, 'sticker', 'hi.webp', 'image/webp', 12, 'https://example.com/hi.webp', ?)`,
		assetID, owner.User["id"].(string), nowMillis(),
	)
	if err != nil {
		t.Fatalf("insert asset: %v", err)
	}

	status, response := api.request(http.MethodPost, "/sticker-packs", owner.Token, map[string]any{
		"scope": "personal",
		"name":  "Idempotent Stickers",
	})
	api.requireStatus(status, http.StatusCreated, response)
	pack := response["pack"].(map[string]any)
	packID := pack["id"].(string)

	body := map[string]any{
		"asset_id": assetID,
		"name":     "hi",
	}
	headers := map[string]string{"Idempotency-Key": "test-add-sticker-key"}
	status, response = api.requestWithHeaders(http.MethodPost, "/sticker-packs/"+packID+"/stickers", owner.Token, body, headers)
	api.requireStatus(status, http.StatusCreated, response)
	firstSticker := response["sticker"].(map[string]any)

	status, response = api.requestWithHeaders(http.MethodPost, "/sticker-packs/"+packID+"/stickers", owner.Token, body, headers)
	api.requireStatus(status, http.StatusCreated, response)
	secondSticker := response["sticker"].(map[string]any)
	if secondSticker["id"] != firstSticker["id"] {
		t.Fatalf("expected idempotent replay to return same sticker: first=%v second=%v", firstSticker, secondSticker)
	}

	var count int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM stickers WHERE pack_id = ? AND asset_id = ?`, packID, assetID).Scan(&count); err != nil {
		t.Fatalf("count stickers: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected one sticker row, got %d", count)
	}
}

func TestManageStickersRenameReorderAndDownload(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("sticker_manage_owner")
	other := api.register("sticker_manage_other")

	assets := []struct {
		id       string
		filename string
		mimeType string
		body     []byte
	}{
		{id: "asset_manage_one", filename: "one.webp", mimeType: "image/webp", body: []byte("one-image")},
		{id: "asset_manage_two", filename: "two.png", mimeType: "image/png", body: []byte("two-image")},
	}
	for _, asset := range assets {
		if err := os.MkdirAll(filepath.Join(api.assets, asset.id), 0o755); err != nil {
			t.Fatalf("create asset dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(api.assets, asset.id, asset.filename), asset.body, 0o644); err != nil {
			t.Fatalf("write asset: %v", err)
		}
		_, err := api.db.Exec(
			`INSERT INTO assets (id, owner_user_id, purpose, filename, mime_type, size_bytes, url, created_at)
			 VALUES (?, ?, 'sticker', ?, ?, ?, ?, ?)`,
			asset.id, owner.User["id"].(string), asset.filename, asset.mimeType, len(asset.body), "/assets/"+asset.id+"/"+asset.filename, nowMillis(),
		)
		if err != nil {
			t.Fatalf("insert asset: %v", err)
		}
	}

	status, response := api.request(http.MethodPost, "/sticker-packs", owner.Token, map[string]any{
		"scope": "personal",
		"name":  "Managed Stickers",
	})
	api.requireStatus(status, http.StatusCreated, response)
	packID := response["pack"].(map[string]any)["id"].(string)

	status, response = api.request(http.MethodPost, "/sticker-packs/"+packID+"/stickers", owner.Token, map[string]any{
		"asset_id": assets[0].id,
		"name":     "smile",
	})
	api.requireStatus(status, http.StatusCreated, response)
	first := response["sticker"].(map[string]any)
	firstID := first["id"].(string)
	if first["name"] != "smile" {
		t.Fatalf("first sticker name mismatch: %v", first)
	}

	status, response = api.request(http.MethodPost, "/sticker-packs/"+packID+"/stickers", owner.Token, map[string]any{
		"asset_id": assets[1].id,
		"name":     "smile",
	})
	api.requireStatus(status, http.StatusCreated, response)
	second := response["sticker"].(map[string]any)
	secondID := second["id"].(string)
	if second["name"] != "smile (2)" {
		t.Fatalf("duplicate sticker name should be suffixed: %v", second)
	}

	status, response = api.request(http.MethodPatch, "/sticker-packs/"+packID+"/stickers/"+secondID, owner.Token, map[string]any{
		"name": "smile",
	})
	api.requireStatus(status, http.StatusOK, response)
	renamed := response["sticker"].(map[string]any)
	if renamed["name"] != "smile (2)" {
		t.Fatalf("rename should preserve unique name: %v", renamed)
	}

	status, response = api.request(http.MethodPost, "/sticker-packs/"+packID+"/stickers/reorder", owner.Token, map[string]any{
		"sticker_ids": []string{secondID, firstID},
	})
	api.requireStatus(status, http.StatusOK, response)
	reordered := response["pack"].(map[string]any)["stickers"].([]any)
	if reordered[0].(map[string]any)["id"] != secondID || reordered[1].(map[string]any)["id"] != firstID {
		t.Fatalf("stickers not reordered: %v", reordered)
	}

	single := api.rawRequest(http.MethodGet, "/stickers/download?ids="+firstID, owner.Token, nil, nil)
	if single.Code != http.StatusOK {
		t.Fatalf("single download status=%d body=%q", single.Code, single.Body.String())
	}
	if single.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("single download content type mismatch: %s", single.Header().Get("Content-Type"))
	}
	if !bytes.Equal(single.Body.Bytes(), assets[0].body) {
		t.Fatalf("single download body mismatch: %q", single.Body.Bytes())
	}

	denied := api.rawRequest(http.MethodGet, "/stickers/download?ids="+firstID, other.Token, nil, nil)
	if denied.Code != http.StatusNotFound {
		t.Fatalf("other user should not download personal sticker: status=%d body=%q", denied.Code, denied.Body.String())
	}

	batch := api.rawRequest(http.MethodGet, "/stickers/download?ids="+secondID+","+firstID, owner.Token, nil, nil)
	if batch.Code != http.StatusOK {
		t.Fatalf("batch download status=%d body=%q", batch.Code, batch.Body.String())
	}
	if batch.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("batch download content type mismatch: %s", batch.Header().Get("Content-Type"))
	}
	archive, err := zip.NewReader(bytes.NewReader(batch.Body.Bytes()), int64(batch.Body.Len()))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	if len(archive.File) != 2 {
		t.Fatalf("zip should contain two files: %v", archive.File)
	}
	if archive.File[0].Name != "smile (2).png" || archive.File[1].Name != "smile.webp" {
		t.Fatalf("zip entry names should follow selected order and sticker names: %v, %v", archive.File[0].Name, archive.File[1].Name)
	}

	status, response = api.request(http.MethodDelete, "/sticker-packs/"+packID+"/stickers/"+firstID, owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodDelete, "/sticker-packs/"+packID+"/stickers/"+firstID, owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	var remaining int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM stickers WHERE id = ?`, firstID).Scan(&remaining); err != nil {
		t.Fatalf("count deleted sticker: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("deleted sticker still exists")
	}
}

func TestUploadImageStoresAssetFile(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("asset_owner")

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("purpose", "avatar"); err != nil {
		t.Fatalf("write purpose: %v", err)
	}
	part, err := writer.CreateFormFile("file", "avatar.png")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	pngBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	if _, err := part.Write(pngBytes); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads/images", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+owner.Token)
	rec := httptest.NewRecorder()
	api.router.ServeHTTP(rec, req)

	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v body=%q", err, rec.Body.String())
	}
	api.requireStatus(rec.Code, http.StatusCreated, response)
	asset := response["asset"].(map[string]any)
	if asset["mime_type"] != "image/png" {
		t.Fatalf("expected sniffed image/png mime: %v", asset)
	}
	assetID := asset["id"].(string)
	var filename string
	if err := api.db.QueryRow(`SELECT filename FROM assets WHERE id = ?`, assetID).Scan(&filename); err != nil {
		t.Fatalf("read asset row: %v", err)
	}
	saved, err := os.ReadFile(filepath.Join(api.assets, assetID, filename))
	if err != nil {
		t.Fatalf("read saved asset: %v", err)
	}
	if !bytes.Equal(saved, pngBytes) {
		t.Fatalf("saved asset bytes changed: %v", saved)
	}
}

func TestUploadFileStoresAssetFile(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("file_asset_owner")

	fileBytes := []byte("%PDF-1.7\nhello")
	status, response := api.uploadMultipart("/uploads/files", owner.Token, "../report 2026?.pdf", "application/pdf", "", fileBytes)
	api.requireStatus(status, http.StatusCreated, response)

	asset := response["asset"].(map[string]any)
	assetID := asset["id"].(string)
	if asset["filename"] != "report-2026.pdf" {
		t.Fatalf("filename should be sanitized: %v", asset)
	}
	if asset["mime_type"] != "application/pdf" {
		t.Fatalf("expected sniffed application/pdf mime: %v", asset)
	}
	if int64(asset["size_bytes"].(float64)) != int64(len(fileBytes)) {
		t.Fatalf("size_bytes mismatch: %v", asset)
	}
	if asset["thumbnail_url"] != nil {
		t.Fatalf("non-image upload should not expose thumbnail_url: %v", asset)
	}

	var purpose, filename, storageKey string
	if err := api.db.QueryRow(`SELECT purpose, filename, storage_key FROM assets WHERE id = ?`, assetID).Scan(&purpose, &filename, &storageKey); err != nil {
		t.Fatalf("read asset row: %v", err)
	}
	if purpose != "message_file" {
		t.Fatalf("default file purpose mismatch: %q", purpose)
	}
	if storageKey != "assets/"+assetID+"/"+filename {
		t.Fatalf("storage key mismatch: %q", storageKey)
	}
	saved, err := os.ReadFile(filepath.Join(api.assets, assetID, filename))
	if err != nil {
		t.Fatalf("read saved asset: %v", err)
	}
	if !bytes.Equal(saved, fileBytes) {
		t.Fatalf("saved asset bytes changed: %v", saved)
	}
}

func TestUploadImageRejectsNonImage(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("non_image_owner")

	status, response := api.uploadMultipart("/uploads/images", owner.Token, "notes.txt", "text/plain", "", []byte("not an image"))
	api.requireStatus(status, http.StatusBadRequest, response)

	var count int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM assets`).Scan(&count); err != nil {
		t.Fatalf("count assets: %v", err)
	}
	if count != 0 {
		t.Fatalf("non-image upload should not create asset rows, got %d", count)
	}
}

func TestUploadFileRejectsOverLimit(t *testing.T) {
	api := newAPIHarness(t)
	api.cfg.AssetUploadMaxBytes = 4
	owner := api.register("file_limit_owner")

	status, response := api.uploadMultipart("/uploads/files", owner.Token, "tiny.bin", "application/octet-stream", "", []byte("12345"))
	api.requireStatus(status, http.StatusRequestEntityTooLarge, response)

	var count int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM assets`).Scan(&count); err != nil {
		t.Fatalf("count assets: %v", err)
	}
	if count != 0 {
		t.Fatalf("over-limit upload should not create asset rows, got %d", count)
	}
}

func TestApprovalRequiredJoinFlow(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("approval_owner")
	joiner := api.register("approval_joiner")
	room := api.createRoom(owner.Token, map[string]any{"name": "Approval Room"})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", joiner.Token, map[string]any{
		"reason": "  I work with this team.  ",
	})
	api.requireStatus(status, http.StatusAccepted, response)
	joinRequest := response["join_request"].(map[string]any)
	if joinRequest["status"] != "pending" {
		t.Fatalf("join request should be pending: %v", joinRequest)
	}
	if joinRequest["reason"] != "I work with this team." {
		t.Fatalf("join request should include trimmed reason: %v", joinRequest)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/join-requests?status=pending", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	requests := response["requests"].([]any)
	if len(requests) != 1 || requests[0].(map[string]any)["id"] != joinRequest["id"] {
		t.Fatalf("pending join request not listed: %v", response)
	}
	if requests[0].(map[string]any)["reason"] != "I work with this team." {
		t.Fatalf("admin join request should include reason: %v", response)
	}
	if requests[0].(map[string]any)["source"] != "public_search" {
		t.Fatalf("direct application should be marked as public search: %v", response)
	}
	if got := len(requests[0].(map[string]any)["inviters"].([]any)); got != 0 {
		t.Fatalf("direct application should not list inviters, got %d: %v", got, response)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/join-requests/"+joinRequest["id"].(string), owner.Token, map[string]any{"decision": "approve"})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/rooms/"+roomID, joiner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	approvedRoom := response["room"].(map[string]any)
	membership := approvedRoom["my_membership"].(map[string]any)
	if membership["role"] != "member" {
		t.Fatalf("approved joiner should become member: %v", membership)
	}
}

func TestRoomInviteHistoryClearedWhenTargetLeavesOrIsRemoved(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("invite_clear_owner")
	leaver := api.register("invite_clear_leaver")
	removed := api.register("invite_clear_removed")
	room := api.createRoom(owner.Token, map[string]any{"name": "Invite Cleanup", "join_policy": "approval_required"})
	roomID := room["id"].(string)

	assertNoInviteHistory := func(userID string) {
		t.Helper()
		var count int
		if err := api.db.QueryRow(
			`SELECT COUNT(*) FROM room_invites WHERE room_id = ? AND target_user_id = ?`,
			roomID,
			userID,
		).Scan(&count); err != nil {
			t.Fatalf("count invite history: %v", err)
		}
		if count != 0 {
			t.Fatalf("invite history should be cleared for %s, got %d rows", userID, count)
		}
	}

	insertStaleInviteHistory := func(userID string) {
		t.Helper()
		old := nowMillis() - 60000
		if _, err := api.db.Exec(
			`INSERT INTO room_invites (id, room_id, inviter_user_id, target_user_id, status, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'accepted', ?, ?)`,
			newID("rinv"),
			roomID,
			owner.User["id"].(string),
			userID,
			old,
			old,
		); err != nil {
			t.Fatalf("insert stale invite history: %v", err)
		}
	}

	assertDirectApplication := func(token, userID string) string {
		t.Helper()
		status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", token, map[string]any{"reason": "again"})
		api.requireStatus(status, http.StatusAccepted, response)
		requestID := response["join_request"].(map[string]any)["id"].(string)

		status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/join-requests?status=pending", owner.Token, nil)
		api.requireStatus(status, http.StatusOK, response)
		for _, item := range response["requests"].([]any) {
			request := item.(map[string]any)
			user := request["user"].(map[string]any)
			if user["id"] != userID {
				continue
			}
			if request["source"] != "public_search" {
				t.Fatalf("stale invite source should not survive leave/remove: %v", request)
			}
			if got := len(request["inviters"].([]any)); got != 0 {
				t.Fatalf("stale inviters should be cleared, got %d: %v", got, request)
			}
			return requestID
		}
		t.Fatalf("join request for %s not found: %v", userID, response)
		return ""
	}

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": leaver.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	status, response = api.request(http.MethodPatch, "/room-invites/"+response["invite"].(map[string]any)["id"].(string), leaver.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/leave", leaver.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	assertNoInviteHistory(leaver.User["id"].(string))
	insertStaleInviteHistory(leaver.User["id"].(string))
	requestID := assertDirectApplication(leaver.Token, leaver.User["id"].(string))
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/join-requests/"+requestID, owner.Token, map[string]any{"decision": "approve"})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": removed.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	status, response = api.request(http.MethodPatch, "/room-invites/"+response["invite"].(map[string]any)["id"].(string), removed.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodDelete, "/rooms/"+roomID+"/members/"+removed.User["id"].(string), owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	assertNoInviteHistory(removed.User["id"].(string))
	insertStaleInviteHistory(removed.User["id"].(string))
	assertDirectApplication(removed.Token, removed.User["id"].(string))
}

func TestRoomApplicationNotifications(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("application_owner")
	joiner := api.register("application_joiner")
	adminReviewer := api.register("app_deleted_reviewer")
	deletedReviewerJoiner := api.register("app_deleted_joiner")
	room := api.createRoom(owner.Token, map[string]any{"name": "Application Room", "description": "Application room bio"})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", joiner.Token, map[string]any{
		"reason": "Please let me in",
	})
	api.requireStatus(status, http.StatusAccepted, response)
	requestID := response["join_request"].(map[string]any)["id"].(string)

	status, response = api.request(http.MethodGet, "/room-applications?status=all", joiner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	applications := response["applications"].([]any)
	if len(applications) != 1 {
		t.Fatalf("pending application should be listed: %v", response)
	}
	application := applications[0].(map[string]any)
	if application["status"] != "pending" || application["reviewer"] != nil {
		t.Fatalf("pending application payload mismatch: %v", application)
	}
	if application["reason"] != "Please let me in" {
		t.Fatalf("application should include reason: %v", application)
	}
	if application["room"].(map[string]any)["name"] != "Application Room" {
		t.Fatalf("application should include room payload: %v", application)
	}
	pendingRoom := application["room"].(map[string]any)
	if pendingRoom["description"] != "Application room bio" {
		t.Fatalf("application room should include description: %v", pendingRoom)
	}
	if pendingRoom["created_by"].(map[string]any)["id"] != owner.User["id"] {
		t.Fatalf("application room should include creator: %v", pendingRoom)
	}
	if _, ok := pendingRoom["my_membership"]; ok {
		t.Fatalf("pending application should not include viewer room membership: %v", pendingRoom)
	}

	status, response = api.request(http.MethodPatch, "/room-applications/"+requestID, joiner.Token, map[string]any{"decision": "withdraw"})
	api.requireStatus(status, http.StatusOK, response)
	if response["application"].(map[string]any)["status"] != "withdrawn" {
		t.Fatalf("withdraw should mark application withdrawn: %v", response)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/join-requests?status=pending", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["requests"].([]any)); got != 0 {
		t.Fatalf("withdrawn application should leave admin queue, got %d: %v", got, response)
	}

	status, response = api.request(http.MethodPatch, "/room-applications/"+requestID, joiner.Token, map[string]any{"decision": "withdraw"})
	api.requireStatus(status, http.StatusConflict, response)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", joiner.Token, nil)
	api.requireStatus(status, http.StatusAccepted, response)
	requestID = response["join_request"].(map[string]any)["id"].(string)

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/join-requests/"+requestID, owner.Token, map[string]any{"decision": "approve"})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/room-applications?status=all", joiner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	applications = response["applications"].([]any)
	if len(applications) != 1 {
		t.Fatalf("approved application should remain listed: %v", response)
	}
	application = applications[0].(map[string]any)
	reviewer := application["reviewer"].(map[string]any)
	if application["status"] != "approved" || application["reviewed_at"] == nil || reviewer["id"] != owner.User["id"] {
		t.Fatalf("approved application should include reviewer and reviewed_at: %v", application)
	}
	if reviewer["room_role"] != "owner" {
		t.Fatalf("reviewer should include room role: %v", reviewer)
	}
	approvedRoom := application["room"].(map[string]any)
	if approvedRoom["joined"] != true {
		t.Fatalf("approved application room should be marked joined: %v", approvedRoom)
	}
	if approvedRoom["my_membership"].(map[string]any)["role"] != "member" {
		t.Fatalf("approved application room should include viewer membership: %v", approvedRoom)
	}
	if _, ok := approvedRoom["personal_profile"].(map[string]any); !ok {
		t.Fatalf("approved application room should include viewer room profile: %v", approvedRoom)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": adminReviewer.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	adminInviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodPatch, "/room-invites/"+adminInviteID, adminReviewer.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+adminReviewer.User["id"].(string), owner.Token, map[string]any{
		"role": "admin",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", deletedReviewerJoiner.Token, nil)
	api.requireStatus(status, http.StatusAccepted, response)
	deletedReviewerRequestID := response["join_request"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/join-requests/"+deletedReviewerRequestID, adminReviewer.Token, map[string]any{
		"decision": "approve",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodDelete, "/users/me/account", adminReviewer.Token, map[string]any{
		"confirm": true,
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/room-applications?status=all", deletedReviewerJoiner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	applications = response["applications"].([]any)
	if len(applications) != 1 || applications[0].(map[string]any)["id"] != deletedReviewerRequestID {
		t.Fatalf("application reviewed by deleted reviewer should remain listed: %v", response)
	}
	application = applications[0].(map[string]any)
	if application["reviewer_exists"] != false {
		t.Fatalf("deleted reviewer application should mark reviewer missing: %v", application)
	}
	deletedReviewer := application["reviewer"].(map[string]any)
	if deletedReviewer["display_name"] != "用户不存在" || deletedReviewer["avatar_url"] != nil {
		t.Fatalf("deleted reviewer summary should be a placeholder: %v", deletedReviewer)
	}
}

func TestRoomInviteFlow(t *testing.T) {
	api := newAPIHarness(t)
	assertRoomInvitesKeepDeletedRooms(t, api.db)
	super := api.login("GANG", "64n9-Ch47")
	owner := api.register("invite_owner")
	joiner := api.register("invite_joiner")
	applicantThenInvited := api.register("invite_applicant_then_invited")
	pendingUser := api.register("invite_pending")
	rejecter := api.register("invite_rejecter")
	multiTarget := api.register("invite_multi_target")
	rejectMultiTarget := api.register("invite_reject_multi_target")
	openInviter := api.register("invite_open_inviter")
	openTarget := api.register("invite_open_target")
	leftInviter := api.register("invite_left_inviter")
	leftTarget := api.register("invite_left_target")
	deletedInviter := api.register("invite_deleted_inviter")
	deletedInviterTarget := api.register("invite_deleted_inviter_target")
	deletedRoomTarget := api.register("invite_deleted_room_target")
	superTarget := api.register("invite_super_target")
	closedRoom := api.createRoom(owner.Token, map[string]any{"name": "Closed Invite Room", "join_policy": "closed"})
	status, response := api.request(http.MethodPost, "/rooms/"+closedRoom["id"].(string)+"/invites", owner.Token, map[string]any{
		"user_id": joiner.User["id"].(string),
	})
	api.requireStatus(status, http.StatusForbidden, response)

	room := api.createRoom(owner.Token, map[string]any{"name": "Invite Room", "join_policy": "approval_required"})
	roomID := room["id"].(string)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": joiner.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	invite := response["invite"].(map[string]any)
	if invite["status"] != "pending" {
		t.Fatalf("invite should be pending: %v", invite)
	}
	if invite["room"].(map[string]any)["name"] != "Invite Room" {
		t.Fatalf("invite should include room summary: %v", invite)
	}
	if invite["inviter"].(map[string]any)["id"] != owner.User["id"] {
		t.Fatalf("invite should include inviter summary: %v", invite)
	}
	if invite["inviter"].(map[string]any)["room_role"] != "owner" {
		t.Fatalf("invite should include inviter room role: %v", invite)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/members?limit=50", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["members"].([]any)); got != 1 {
		t.Fatalf("inviting should not add a membership yet, got %d: %v", got, response)
	}

	status, response = api.request(http.MethodGet, "/room-invites?status=pending", joiner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	invites := response["invites"].([]any)
	if len(invites) != 1 || invites[0].(map[string]any)["id"] != invite["id"] {
		t.Fatalf("pending invite not listed for target: %v", response)
	}

	status, response = api.request(http.MethodPatch, "/room-invites/"+invite["id"].(string), joiner.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusOK, response)
	acceptedRoom := response["room"].(map[string]any)
	membership := acceptedRoom["my_membership"].(map[string]any)
	if membership["role"] != "member" {
		t.Fatalf("accepted invite should add member role: %v", response)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", applicantThenInvited.Token, map[string]any{
		"reason": "Please let me in first",
	})
	api.requireStatus(status, http.StatusAccepted, response)
	pendingBeforeInviteID := response["join_request"].(map[string]any)["id"].(string)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": applicantThenInvited.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	applicantInviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodPatch, "/room-invites/"+applicantInviteID, applicantThenInvited.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusOK, response)
	if response["room"].(map[string]any)["my_membership"].(map[string]any)["role"] != "member" {
		t.Fatalf("admin invite should directly add applicant as member: %v", response)
	}
	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/join-requests?status=pending", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["requests"].([]any)); got != 0 {
		t.Fatalf("admin invite acceptance should clear pending join request, got %d: %v", got, response)
	}
	status, response = api.request(http.MethodGet, "/room-applications?status=all", applicantThenInvited.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	applications := response["applications"].([]any)
	if len(applications) != 1 || applications[0].(map[string]any)["id"] != pendingBeforeInviteID {
		t.Fatalf("approved application should remain in applicant history: %v", response)
	}
	if applications[0].(map[string]any)["status"] != "approved" {
		t.Fatalf("admin invite should mark existing application approved: %v", applications[0])
	}

	secondRoom := api.createRoom(owner.Token, map[string]any{"name": "Invite Room 2", "join_policy": "approval_required"})
	secondRoomID := secondRoom["id"].(string)
	status, response = api.request(http.MethodPost, "/rooms/"+secondRoomID+"/invites", owner.Token, map[string]any{
		"user_id": joiner.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	secondInviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodGet, "/room-invites?status=all", joiner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	allInvites := response["invites"].([]any)
	if len(allInvites) != 2 {
		t.Fatalf("all invites should include pending and processed invites: %v", response)
	}
	if allInvites[0].(map[string]any)["id"] != secondInviteID || allInvites[0].(map[string]any)["status"] != "pending" {
		t.Fatalf("pending invite should sort before processed invites: %v", response)
	}
	if allInvites[1].(map[string]any)["id"] != invite["id"] || allInvites[1].(map[string]any)["status"] != "accepted" {
		t.Fatalf("processed invite should remain visible after pending invites: %v", response)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", joiner.Token, map[string]any{
		"user_id": pendingUser.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	if response["invite"].(map[string]any)["inviter"].(map[string]any)["id"] != joiner.User["id"] {
		t.Fatalf("normal member should be able to invite users: %v", response)
	}
	if response["invite"].(map[string]any)["inviter"].(map[string]any)["room_role"] != "member" {
		t.Fatalf("normal member invite should include inviter room role: %v", response)
	}
	pendingInviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodPatch, "/room-invites/"+pendingInviteID, pendingUser.Token, map[string]any{
		"decision": "accept",
		"reason":   "Invited by Jordan",
	})
	api.requireStatus(status, http.StatusAccepted, response)
	joinRequest := response["join_request"].(map[string]any)
	if joinRequest["status"] != "pending" {
		t.Fatalf("normal member invite should create pending join request: %v", response)
	}
	if joinRequest["reason"] != "Invited by Jordan" {
		t.Fatalf("normal member invite should carry application reason: %v", response)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/members?limit=50", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["members"].([]any)); got != 3 {
		t.Fatalf("pending invite acceptance should not add a membership, got %d: %v", got, response)
	}
	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/join-requests?status=pending", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	requests := response["requests"].([]any)
	if len(requests) != 1 || requests[0].(map[string]any)["id"] != joinRequest["id"] {
		t.Fatalf("pending invite acceptance should be visible to admins: %v", response)
	}
	if requests[0].(map[string]any)["reason"] != "Invited by Jordan" {
		t.Fatalf("pending invite acceptance should expose reason to admins: %v", response)
	}
	if requests[0].(map[string]any)["source"] != "invitation" {
		t.Fatalf("pending invite acceptance should expose invitation source: %v", response)
	}
	inviters := requests[0].(map[string]any)["inviters"].([]any)
	if len(inviters) != 1 || inviters[0].(map[string]any)["username"] != joiner.User["username"] {
		t.Fatalf("pending invite acceptance should list inviters: %v", response)
	}

	openRoom := api.createRoom(owner.Token, map[string]any{"name": "Open Invite Room", "join_policy": "open"})
	openRoomID := openRoom["id"].(string)
	status, response = api.request(http.MethodPost, "/rooms/"+openRoomID+"/invites", owner.Token, map[string]any{
		"user_id": openInviter.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	status, response = api.request(http.MethodPatch, "/room-invites/"+response["invite"].(map[string]any)["id"].(string), openInviter.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPost, "/rooms/"+openRoomID+"/invites", openInviter.Token, map[string]any{
		"user_id": openTarget.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	openInviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodPatch, "/room-invites/"+openInviteID, openTarget.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusOK, response)
	openAcceptedRoom := response["room"].(map[string]any)
	if openAcceptedRoom["my_membership"].(map[string]any)["role"] != "member" {
		t.Fatalf("open room invite should directly add the target as member: %v", response)
	}
	status, response = api.request(http.MethodGet, "/rooms/"+openRoomID+"/join-requests?status=pending", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["requests"].([]any)); got != 0 {
		t.Fatalf("open room invite acceptance should not require approval, got %d requests: %v", got, response)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", joiner.Token, map[string]any{
		"user_id": rejecter.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	rejectedInviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodPatch, "/room-invites/"+rejectedInviteID, rejecter.Token, map[string]any{
		"decision": "reject",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/room-invites?status=all", rejecter.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	rejecterInvites := response["invites"].([]any)
	if len(rejecterInvites) != 1 || rejecterInvites[0].(map[string]any)["status"] != "rejected" {
		t.Fatalf("all invites should include rejected invite: %v", response)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/members?limit=50", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["members"].([]any)); got != 3 {
		t.Fatalf("rejected invite should not add a membership, got %d: %v", got, response)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": multiTarget.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	privilegedInviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": multiTarget.User["id"].(string),
	})
	api.requireStatus(status, http.StatusOK, response)
	if duplicateID := response["invite"].(map[string]any)["id"].(string); duplicateID != privilegedInviteID {
		t.Fatalf("same inviter duplicate pending invite should return original invite, got %s want %s", duplicateID, privilegedInviteID)
	}
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", joiner.Token, map[string]any{
		"user_id": multiTarget.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	memberInviteID := response["invite"].(map[string]any)["id"].(string)
	if memberInviteID == privilegedInviteID {
		t.Fatalf("different inviters should create independent invites: %v", response)
	}
	status, response = api.request(http.MethodGet, "/room-invites?status=pending", multiTarget.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	multiPendingInvites := response["invites"].([]any)
	if len(multiPendingInvites) != 2 {
		t.Fatalf("target should see one pending invite per inviter, got %d: %v", len(multiPendingInvites), response)
	}
	status, response = api.request(http.MethodPatch, "/room-invites/"+memberInviteID, multiTarget.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusOK, response)
	multiAcceptedRoom := response["room"].(map[string]any)
	if multiAcceptedRoom["my_membership"].(map[string]any)["role"] != "member" {
		t.Fatalf("admin invite in same room should allow direct join: %v", response)
	}
	status, response = api.request(http.MethodGet, "/room-invites?status=all", multiTarget.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	acceptedStatuses := map[string]string{}
	for _, item := range response["invites"].([]any) {
		inviteItem := item.(map[string]any)
		acceptedStatuses[inviteItem["id"].(string)] = inviteItem["status"].(string)
	}
	if acceptedStatuses[privilegedInviteID] != "accepted" || acceptedStatuses[memberInviteID] != "accepted" {
		t.Fatalf("accepting one same-room invite should accept every same-room pending invite: %v", response)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": rejectMultiTarget.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	ownerRejectInviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", joiner.Token, map[string]any{
		"user_id": rejectMultiTarget.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	memberRejectInviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodPatch, "/room-invites/"+memberRejectInviteID, rejectMultiTarget.Token, map[string]any{
		"decision": "reject",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/room-invites?status=all", rejectMultiTarget.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	rejectStatuses := map[string]string{}
	for _, item := range response["invites"].([]any) {
		inviteItem := item.(map[string]any)
		rejectStatuses[inviteItem["id"].(string)] = inviteItem["status"].(string)
	}
	if rejectStatuses[memberRejectInviteID] != "rejected" || rejectStatuses[ownerRejectInviteID] != "pending" {
		t.Fatalf("rejecting should only update the selected invite: %v", response)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": leftInviter.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	leftInviterInviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodPatch, "/room-invites/"+leftInviterInviteID, leftInviter.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", leftInviter.Token, map[string]any{
		"user_id": leftTarget.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	leftInviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/leave", leftInviter.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/room-invites?status=pending", leftTarget.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	invites = response["invites"].([]any)
	if len(invites) != 1 || invites[0].(map[string]any)["id"] != leftInviteID {
		t.Fatalf("pending invite from left inviter should remain listed: %v", response)
	}
	leftInvite := invites[0].(map[string]any)
	if leftInvite["invalid_reason"] != "inviter_left" || leftInvite["inviter"].(map[string]any)["room_role"] != "left" {
		t.Fatalf("left inviter invite should be invalid and show left role: %v", leftInvite)
	}
	status, response = api.request(http.MethodPatch, "/room-invites/"+leftInviteID, leftTarget.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusConflict, response)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": deletedInviter.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	deletedInviterJoinInviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodPatch, "/room-invites/"+deletedInviterJoinInviteID, deletedInviter.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", deletedInviter.Token, map[string]any{
		"user_id": deletedInviterTarget.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	deletedInviterInviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodDelete, "/users/me/account", deletedInviter.Token, map[string]any{
		"confirm": true,
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/room-invites?status=pending", deletedInviterTarget.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	invites = response["invites"].([]any)
	if len(invites) != 1 || invites[0].(map[string]any)["id"] != deletedInviterInviteID {
		t.Fatalf("pending invite from deleted inviter should remain listed: %v", response)
	}
	deletedInviterInvite := invites[0].(map[string]any)
	if deletedInviterInvite["inviter_exists"] != false || deletedInviterInvite["invalid_reason"] != "inviter_left" {
		t.Fatalf("deleted inviter invite should be invalid with missing inviter state: %v", deletedInviterInvite)
	}
	deletedInviterSummary := deletedInviterInvite["inviter"].(map[string]any)
	if deletedInviterSummary["display_name"] != "用户不存在" || deletedInviterSummary["avatar_url"] != nil {
		t.Fatalf("deleted inviter summary should be a placeholder: %v", deletedInviterSummary)
	}

	deletedRoom := api.createRoom(owner.Token, map[string]any{"name": "Deleted Invite Room", "join_policy": "approval_required"})
	deletedRoomID := deletedRoom["id"].(string)
	status, response = api.request(http.MethodPost, "/rooms/"+deletedRoomID+"/invites", owner.Token, map[string]any{
		"user_id": deletedRoomTarget.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	deletedInviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodDelete, "/rooms/"+deletedRoomID, owner.Token, map[string]any{
		"confirm_name": "Deleted Invite Room",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/room-invites?status=pending", deletedRoomTarget.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	invites = response["invites"].([]any)
	if len(invites) != 1 || invites[0].(map[string]any)["id"] != deletedInviteID {
		t.Fatalf("pending invite for deleted room should remain listed: %v", response)
	}
	deletedInvite := invites[0].(map[string]any)
	if deletedInvite["room_exists"] != false || deletedInvite["invalid_reason"] != "room_missing" {
		t.Fatalf("deleted room invite should be invalid with room snapshot: %v", deletedInvite)
	}
	if deletedInvite["room"].(map[string]any)["name"] != "Deleted Invite Room" {
		t.Fatalf("deleted room invite should keep room snapshot: %v", deletedInvite)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", super.Token, map[string]any{
		"user_id": superTarget.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	superInvite := response["invite"].(map[string]any)
	if superInvite["invalid_reason"] != nil || superInvite["inviter"].(map[string]any)["room_role"] != "superuser" {
		t.Fatalf("superuser invite should stay valid without room membership: %v", superInvite)
	}
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

// TestListMembersPaginates verifies that listMembers walks every member across
// pages via next_cursor, with no gaps or duplicates, when a room has more
// members than the page limit.
func TestListMembersPaginates(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("page_owner")
	room := api.createRoom(owner.Token, map[string]any{"name": "Paginate", "join_policy": "open"})
	roomID := room["id"].(string)

	want := map[string]bool{owner.User["id"].(string): true}
	const extra = 7
	for i := 0; i < extra; i++ {
		u := api.register(fmt.Sprintf("page_member_%d", i))
		status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", u.Token, nil)
		api.requireStatus(status, http.StatusOK, response)
		want[u.User["id"].(string)] = true
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		path := "/rooms/" + roomID + "/members?limit=3"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		status, response := api.request(http.MethodGet, path, owner.Token, nil)
		api.requireStatus(status, http.StatusOK, response)
		items, _ := response["members"].([]any)
		if len(items) > 3 {
			t.Fatalf("page returned %d members, exceeds limit 3", len(items))
		}
		for _, item := range items {
			m := item.(map[string]any)
			id := m["user"].(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("member %s returned on more than one page", id)
			}
			seen[id] = true
		}
		pages++
		if pages > 20 {
			t.Fatalf("pagination did not terminate")
		}
		next, ok := response["next_cursor"].(string)
		if !ok || next == "" {
			break
		}
		cursor = next
	}

	if len(seen) != len(want) {
		t.Fatalf("paginated over %d members, want %d", len(seen), len(want))
	}
	for id := range want {
		if !seen[id] {
			t.Fatalf("member %s missing from paginated results", id)
		}
	}
}
