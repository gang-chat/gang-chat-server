package chat

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/auth"
	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
	"github.com/zhuangkaiyi/gang-chat/server/internal/db"
	"github.com/zhuangkaiyi/gang-chat/server/internal/idgen"
)

type apiHarness struct {
	t      *testing.T
	router *gin.Engine
	db     *sql.DB
}

type testSession struct {
	Token string
	User  map[string]any
}

func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{
		JWTSecret:              "test-secret",
		AccessTokenTTLSeconds:  900,
		RefreshTokenTTLSeconds: 2592000,
		LoginMaxAttempts:       5,
		LoginWindowSeconds:     900,
		LiveKitHost:            "http://localhost:7880",
	}
	pool := db.Connect(filepath.Join(t.TempDir(), "gang-chat-test.db"))
	t.Cleanup(func() { _ = pool.Close() })

	router := gin.New()
	api := router.Group("/api/v1")
	auth.RegisterRoutes(api, pool, cfg)

	authMW := &auth.AuthMiddleware{DB: pool, JWTSecret: cfg.JWTSecret}
	chatGroup := api.Group("")
	chatGroup.Use(authMW.Handle)
	RegisterRoutes(chatGroup, pool, cfg, nil)

	return &apiHarness{t: t, router: router, db: pool}
}

func (h *apiHarness) request(method, path, token string, body any) (int, map[string]any) {
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
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			h.t.Fatalf("%s %s returned invalid JSON: %v; body=%q", method, path, err, rec.Body.String())
		}
	}
	return rec.Code, decoded
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
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me", member.Token, map[string]any{
		"headphones_muted": false,
	})
	api.requireStatus(status, http.StatusOK, response)
	participant = response["participant"].(map[string]any)
	if participant["voice_blocked"] != false || participant["headphones_muted"] != false {
		t.Fatalf("voice restore should allow headphones again: %v", participant)
	}
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
