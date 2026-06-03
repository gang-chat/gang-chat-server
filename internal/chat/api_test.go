package chat

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
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
	RegisterRoutes(chatGroup, pool, cfg, bus, live)

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
	if joined["role"] != "admin" {
		t.Fatalf("superuser should join closed room as admin: %v", joined)
	}

	status, response = api.request(http.MethodDelete, "/rooms/"+roomID+"/members/"+super.User["id"].(string), owner.Token, nil)
	api.requireStatus(status, http.StatusForbidden, response)
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
	adminCount := 0
	for _, member := range []map[string]any{aliceMember, bobMember} {
		if member["role"] == "admin" {
			adminCount++
		}
	}
	if adminCount != 1 {
		t.Fatalf("exactly one remaining member should be promoted to admin: %v", response)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	rejoined := response["room"].(map[string]any)["my_membership"].(map[string]any)
	if rejoined["role"] != "member" {
		t.Fatalf("departed admin should rejoin as member: %v", rejoined)
	}

	solo := api.createRoom(owner.Token, map[string]any{"name": "Solo", "join_policy": "open"})
	soloID := solo["id"].(string)
	status, response = api.request(http.MethodPost, "/rooms/"+soloID+"/leave", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	var exists int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM rooms WHERE id = ?`, soloID).Scan(&exists); err != nil {
		t.Fatalf("query solo room: %v", err)
	}
	if exists != 0 {
		t.Fatalf("room should be deleted after last member leaves")
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

func TestMusicSessionsAndInvites(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("music_owner")
	member := api.register("music_member")
	guest := api.register("music_guest")
	room := api.createRoom(owner.Token, map[string]any{"name": "Music Room", "join_policy": "open"})
	roomID := room["id"].(string)

	for _, session := range []testSession{member, guest} {
		status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", session.Token, nil)
		api.requireStatus(status, http.StatusOK, response)
	}

	queueStatus, queueResponse := api.request(http.MethodPost, "/rooms/"+roomID+"/music/queue", owner.Token, map[string]any{
		"title":      "Queued Track",
		"artist":     "Test Artist",
		"source_url": "https://example.com/queued.mp3",
	})
	api.requireStatus(queueStatus, http.StatusCreated, queueResponse)
	queue := queueResponse["queue"].([]any)
	if len(queue) != 1 {
		t.Fatalf("queue should have one item: %v", queueResponse)
	}
	queueID := queue[0].(map[string]any)["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/live/join", owner.Token, map[string]any{
		"client_live_session_id": "clive_music_owner",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/join", member.Token, map[string]any{
		"client_live_session_id": "clive_music_member",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/join", guest.Token, map[string]any{
		"client_live_session_id": "clive_music_guest",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/music/session", owner.Token, map[string]any{
		"action":   "play",
		"queue_id": queueID,
	})
	api.requireStatus(status, http.StatusOK, response)
	mySession := response["my_session"].(map[string]any)
	if mySession["state"] != "playing" || mySession["current_queue_id"] != queueID {
		t.Fatalf("owner music session not playing queue item: %v", mySession)
	}
	listeners := response["listeners"].([]any)
	if len(listeners) != 1 {
		t.Fatalf("expected owner as first listener: %v", response)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/live", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	live := response["live"].(map[string]any)
	ownerParticipant := participantByUserID(t, live, owner.User["id"].(string))
	if ownerParticipant["headphones_listening"] != true || ownerParticipant["music_listening"] != true {
		t.Fatalf("live participant should expose listening states: %v", ownerParticipant)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/music/session", member.Token, map[string]any{
		"action":         "follow_user",
		"target_user_id": owner.User["id"].(string),
	})
	api.requireStatus(status, http.StatusOK, response)
	mySession = response["my_session"].(map[string]any)
	if mySession["follow_user_id"] != owner.User["id"].(string) || mySession["current_queue_id"] != queueID {
		t.Fatalf("member should follow owner music session: %v", mySession)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/music/invites", owner.Token, map[string]any{
		"include_all_not_listening": true,
	})
	api.requireStatus(status, http.StatusCreated, response)
	invites := response["invites"].([]any)
	if len(invites) != 1 {
		t.Fatalf("only the live guest should be invited: %v", response)
	}
	invite := invites[0].(map[string]any)
	if invite["target_user_id"] != guest.User["id"].(string) {
		t.Fatalf("invite should target non-listening live user: %v", invite)
	}
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

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", joiner.Token, nil)
	api.requireStatus(status, http.StatusAccepted, response)
	joinRequest := response["join_request"].(map[string]any)
	if joinRequest["status"] != "pending" {
		t.Fatalf("join request should be pending: %v", joinRequest)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/join-requests?status=pending", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	requests := response["requests"].([]any)
	if len(requests) != 1 || requests[0].(map[string]any)["id"] != joinRequest["id"] {
		t.Fatalf("pending join request not listed: %v", response)
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
