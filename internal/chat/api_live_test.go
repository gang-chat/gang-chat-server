package chat

import (
	"net/http"
	"testing"
	"time"
)

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

func TestLiveJoinTokenUsesLongLivedRoomToken(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("live_token_owner")
	room := api.createRoom(owner.Token, map[string]any{"name": "Live Token Room", "join_policy": "open"})
	roomID := room["id"].(string)

	before := time.Now().UTC()
	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/live/join", owner.Token, map[string]any{
		"client_live_session_id": "clive_token_owner",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)

	liveKit, ok := response["livekit"].(map[string]any)
	if !ok {
		t.Fatalf("live join response missing livekit payload: %v", response)
	}
	if liveKit["room_name"] != roomID {
		t.Fatalf("livekit room_name should match room id: %v", liveKit)
	}
	expiresAtRaw, ok := liveKit["token_expires_at"].(string)
	if !ok || expiresAtRaw == "" {
		t.Fatalf("livekit payload missing token_expires_at: %v", liveKit)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresAtRaw)
	if err != nil {
		t.Fatalf("token_expires_at is not RFC3339Nano: %v", err)
	}
	minExpected := before.Add(liveKitJoinTokenTTL - time.Minute)
	maxExpected := before.Add(liveKitJoinTokenTTL + time.Minute)
	if expiresAt.Before(minExpected) || expiresAt.After(maxExpected) {
		t.Fatalf("token expiry got %s, want within [%s, %s]", expiresAt, minExpected, maxExpected)
	}
}

func TestLiveParticipantsUseRoomDisplayNames(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("live_alias_owner")
	member := api.register("live_alias_member")
	room := api.createRoom(owner.Token, map[string]any{"name": "Live Alias Room", "join_policy": "open"})
	roomID := room["id"].(string)
	memberID := member.User["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if _, err := api.db.Exec(
		`UPDATE room_memberships SET room_display_name = ? WHERE room_id = ? AND user_id = ?`,
		"Voice Alias",
		roomID,
		memberID,
	); err != nil {
		t.Fatalf("set room display name: %v", err)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/join", member.Token, map[string]any{
		"client_live_session_id": "clive_alias_member",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)
	participant := response["participant"].(map[string]any)
	user := participant["user"].(map[string]any)
	if user["room_display_name"] != "Voice Alias" || user["room_role"] != "member" {
		t.Fatalf("join participant should include room display fields: %v", participant)
	}
	live := response["live"].(map[string]any)
	participant = participantByUserID(t, live, memberID)
	user = participant["user"].(map[string]any)
	if user["room_display_name"] != "Voice Alias" || user["room_role"] != "member" {
		t.Fatalf("join live snapshot should include room display fields: %v", participant)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/live", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	live = response["live"].(map[string]any)
	participant = participantByUserID(t, live, memberID)
	user = participant["user"].(map[string]any)
	if user["room_display_name"] != "Voice Alias" || user["room_role"] != "member" {
		t.Fatalf("live state should include room display fields: %v", participant)
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
	memberKey := roomID + "/" + member.User["id"].(string)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/participants/"+member.User["id"].(string)+"/moderation", owner.Token, map[string]any{
		"action": "mute_mic",
		"reason": "mic test",
	})
	api.requireStatus(status, http.StatusOK, response)
	if !contains(api.live.mediaPermissions, memberKey+" publish=false subscribe=true") {
		t.Fatalf("mute_mic should revoke publish while preserving subscribe on LiveKit: %v", api.live.mediaPermissions)
	}
	if !contains(api.live.micMuted, memberKey+"=true") {
		t.Fatalf("mute_mic should server-mute the mic on LiveKit: %v", api.live.micMuted)
	}
	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/live", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	live := response["live"].(map[string]any)
	participant := participantByUserID(t, live, member.User["id"].(string))
	if participant["mic_muted"] != true || participant["mic_blocked"] != true || participant["headphones_blocked"] != false {
		t.Fatalf("mute_mic should force only the microphone: %v", participant)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/participants/"+member.User["id"].(string)+"/moderation", owner.Token, map[string]any{
		"action": "restore_voice",
	})
	api.requireStatus(status, http.StatusOK, response)
	if !contains(api.live.mediaPermissions, memberKey+" publish=true subscribe=true") {
		t.Fatalf("restore_voice should re-grant publish while preserving subscribe on LiveKit: %v", api.live.mediaPermissions)
	}
	if !contains(api.live.micMuted, memberKey+"=false") {
		t.Fatalf("restore_voice should server-unmute the mic on LiveKit: %v", api.live.micMuted)
	}
	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/live", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	live = response["live"].(map[string]any)
	participant = participantByUserID(t, live, member.User["id"].(string))
	if participant["mic_muted"] != false || participant["mic_blocked"] != false ||
		participant["voice_blocked"] != false || participant["headphones_blocked"] != false {
		t.Fatalf("restore_voice should restore only the microphone after mute_mic: %v", participant)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me", member.Token, map[string]any{
		"headphones_muted": true,
	})
	api.requireStatus(status, http.StatusOK, response)
	participant = response["participant"].(map[string]any)
	if participant["headphones_muted"] != true {
		t.Fatalf("headphones should be muted: %v", participant)
	}

	mediaWritesBeforeHeadphonesMute := len(api.live.mediaPermissions)
	micWritesBeforeHeadphonesMute := len(api.live.micMuted)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/participants/"+member.User["id"].(string)+"/moderation", owner.Token, map[string]any{
		"action": "block_voice",
		"reason": "test",
	})
	api.requireStatus(status, http.StatusOK, response)

	// block_voice is now the headphone-mute primitive: it revokes subscribe
	// without touching microphone publish or server-side mic mute.
	if len(api.live.mediaPermissions) != mediaWritesBeforeHeadphonesMute+1 ||
		api.live.mediaPermissions[len(api.live.mediaPermissions)-1] != memberKey+" publish=true subscribe=false" {
		t.Fatalf("block_voice should preserve publish and revoke only subscribe on LiveKit: %v", api.live.mediaPermissions)
	}
	if len(api.live.micMuted) != micWritesBeforeHeadphonesMute {
		t.Fatalf("block_voice should not server-mute the mic on LiveKit: %v", api.live.micMuted)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/live", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	live = response["live"].(map[string]any)
	participant = participantByUserID(t, live, member.User["id"].(string))
	if participant["mic_muted"] != false || participant["mic_blocked"] != false ||
		participant["headphones_muted"] != true || participant["headphones_blocked"] != true ||
		participant["headphones_listening"] != false || participant["voice_blocked"] != false {
		t.Fatalf("headphones mute should force only listening off: %v", participant)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me", member.Token, map[string]any{
		"mic_muted":        false,
		"headphones_muted": false,
	})
	api.requireStatus(status, http.StatusOK, response)
	participant = response["participant"].(map[string]any)
	if participant["mic_muted"] != false || participant["mic_blocked"] != false ||
		participant["headphones_muted"] != true || participant["headphones_blocked"] != true ||
		participant["voice_blocked"] != false {
		t.Fatalf("headphones-muted user should still be allowed to use mic: %v", participant)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/participants/"+member.User["id"].(string)+"/moderation", owner.Token, map[string]any{
		"action": "restore_headphones",
	})
	api.requireStatus(status, http.StatusOK, response)
	if !contains(api.live.mediaPermissions, memberKey+" publish=true subscribe=true") {
		t.Fatalf("restore_headphones should re-grant subscribe while preserving publish on LiveKit: %v", api.live.mediaPermissions)
	}
	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/live", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	live = response["live"].(map[string]any)
	participant = participantByUserID(t, live, member.User["id"].(string))
	if participant["mic_blocked"] != false || participant["headphones_blocked"] != false ||
		participant["headphones_muted"] != false || participant["headphones_listening"] != true {
		t.Fatalf("restore_headphones should restore only listening: %v", participant)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/participants/"+member.User["id"].(string)+"/moderation", owner.Token, map[string]any{
		"action": "mute_mic",
	})
	api.requireStatus(status, http.StatusOK, response)
	if api.live.mediaPermissions[len(api.live.mediaPermissions)-1] != memberKey+" publish=false subscribe=true" {
		t.Fatalf("mute_mic should keep headphones permission while blocking mic: %v", api.live.mediaPermissions)
	}
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/participants/"+member.User["id"].(string)+"/moderation", owner.Token, map[string]any{
		"action": "block_voice",
	})
	api.requireStatus(status, http.StatusOK, response)
	if api.live.mediaPermissions[len(api.live.mediaPermissions)-1] != memberKey+" publish=false subscribe=false" {
		t.Fatalf("headphones mute should preserve existing mic block: %v", api.live.mediaPermissions)
	}
	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/live", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	live = response["live"].(map[string]any)
	participant = participantByUserID(t, live, member.User["id"].(string))
	if participant["mic_blocked"] != true || participant["headphones_blocked"] != true ||
		participant["voice_blocked"] != true || participant["headphones_muted"] != true {
		t.Fatalf("combined mic/headphones mute should mark both independently: %v", participant)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/participants/"+member.User["id"].(string)+"/moderation", owner.Token, map[string]any{
		"action": "restore_voice",
	})
	api.requireStatus(status, http.StatusOK, response)
	if api.live.mediaPermissions[len(api.live.mediaPermissions)-1] != memberKey+" publish=true subscribe=false" {
		t.Fatalf("restore_voice should preserve headphones mute: %v", api.live.mediaPermissions)
	}
	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/live", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	live = response["live"].(map[string]any)
	participant = participantByUserID(t, live, member.User["id"].(string))
	if participant["mic_blocked"] != false || participant["voice_blocked"] != false ||
		participant["headphones_blocked"] != true || participant["headphones_muted"] != true {
		t.Fatalf("restore_voice should leave headphones muted: %v", participant)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/participants/"+member.User["id"].(string)+"/moderation", owner.Token, map[string]any{
		"action": "restore_headphones",
	})
	api.requireStatus(status, http.StatusOK, response)
	if api.live.mediaPermissions[len(api.live.mediaPermissions)-1] != memberKey+" publish=true subscribe=true" {
		t.Fatalf("restore_headphones should preserve restored mic permission: %v", api.live.mediaPermissions)
	}
	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/live", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	live = response["live"].(map[string]any)
	participant = participantByUserID(t, live, member.User["id"].(string))
	if participant["mic_blocked"] != false || participant["voice_blocked"] != false ||
		participant["headphones_blocked"] != false || participant["headphones_muted"] != false {
		t.Fatalf("restore_headphones should clear the remaining headphones mute: %v", participant)
	}

	// A headphones mute must outlive the live session in the same room without
	// carrying a microphone mute along with it.
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
	if participant["voice_blocked"] != false || participant["mic_blocked"] != false ||
		participant["headphones_blocked"] != true || participant["headphones_muted"] != true ||
		participant["mic_muted"] != true {
		t.Fatalf("headphones mute should persist across rejoin without mic block: %v", participant)
	}
}

func TestLiveCameraAndScreenShareAreExclusive(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("media_owner")
	room := api.createRoom(owner.Token, map[string]any{"name": "Media Room", "join_policy": "open"})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/live/join", owner.Token, map[string]any{
		"client_live_session_id": "clive_media_owner",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me", owner.Token, map[string]any{
		"camera_on": true,
	})
	api.requireStatus(status, http.StatusOK, response)
	participant := response["participant"].(map[string]any)
	if participant["camera_on"] != true || participant["screen_sharing"] != false {
		t.Fatalf("camera should disable screen share: %v", participant)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me", owner.Token, map[string]any{
		"screen_sharing": true,
	})
	api.requireStatus(status, http.StatusOK, response)
	participant = response["participant"].(map[string]any)
	if participant["camera_on"] != false || participant["screen_sharing"] != true {
		t.Fatalf("screen share should disable camera: %v", participant)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me", owner.Token, map[string]any{
		"camera_on":      true,
		"screen_sharing": true,
	})
	api.requireStatus(status, http.StatusOK, response)
	participant = response["participant"].(map[string]any)
	if participant["camera_on"] != false || participant["screen_sharing"] != true {
		t.Fatalf("simultaneous true media flags should prefer screen share: %v", participant)
	}
}

func TestLiveModerationPersistsOnlyWithinRoom(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("voice_scope_owner")
	member := api.register("voice_scope_member")
	roomA := api.createRoom(owner.Token, map[string]any{"name": "Voice Scope A", "join_policy": "open"})
	roomB := api.createRoom(owner.Token, map[string]any{"name": "Voice Scope B", "join_policy": "open"})
	roomAID := roomA["id"].(string)
	roomBID := roomB["id"].(string)
	memberID := member.User["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomAID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomBID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPost, "/rooms/"+roomAID+"/live/join", member.Token, map[string]any{
		"client_live_session_id": "clive_scope_a_initial",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPost, "/rooms/"+roomAID+"/live/participants/"+memberID+"/moderation", owner.Token, map[string]any{
		"action": "mute_mic",
		"reason": "room scoped mute",
	})
	api.requireStatus(status, http.StatusOK, response)

	if _, err := api.db.Exec(`DELETE FROM live_participants WHERE room_id = ? AND user_id = ?`, roomAID, memberID); err != nil {
		t.Fatalf("failed to clear room A live participant after mute: %v", err)
	}
	status, response = api.request(http.MethodPost, "/rooms/"+roomAID+"/live/join", member.Token, map[string]any{
		"client_live_session_id": "clive_scope_a_after_mute",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)
	participant := response["participant"].(map[string]any)
	if participant["mic_blocked"] != true || participant["mic_muted"] != true ||
		participant["voice_blocked"] != true || participant["headphones_blocked"] != false {
		t.Fatalf("mic mute should persist only as a microphone block in the same room: %v", participant)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomBID+"/live/join", member.Token, map[string]any{
		"client_live_session_id": "clive_scope_b_after_room_a_mute",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)
	participant = response["participant"].(map[string]any)
	if participant["mic_blocked"] != false || participant["voice_blocked"] != false ||
		participant["headphones_blocked"] != false {
		t.Fatalf("room A mic mute should not affect room B: %v", participant)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomAID+"/live/participants/"+memberID+"/moderation", owner.Token, map[string]any{
		"action": "restore_voice",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPost, "/rooms/"+roomAID+"/live/participants/"+memberID+"/moderation", owner.Token, map[string]any{
		"action": "block_voice",
		"reason": "room scoped headphones mute",
	})
	api.requireStatus(status, http.StatusOK, response)

	if _, err := api.db.Exec(`DELETE FROM live_participants WHERE room_id = ? AND user_id = ?`, roomAID, memberID); err != nil {
		t.Fatalf("failed to clear room A live participant after headphones mute: %v", err)
	}
	status, response = api.request(http.MethodPost, "/rooms/"+roomAID+"/live/join", member.Token, map[string]any{
		"client_live_session_id": "clive_scope_a_after_headphones_mute",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)
	participant = response["participant"].(map[string]any)
	if participant["mic_blocked"] != false || participant["voice_blocked"] != false ||
		participant["headphones_blocked"] != true || participant["headphones_muted"] != true ||
		participant["mic_muted"] != true {
		t.Fatalf("headphones mute should persist in the same room without mic block: %v", participant)
	}

	if _, err := api.db.Exec(`DELETE FROM live_participants WHERE room_id = ? AND user_id = ?`, roomBID, memberID); err != nil {
		t.Fatalf("failed to clear room B live participant: %v", err)
	}
	status, response = api.request(http.MethodPost, "/rooms/"+roomBID+"/live/join", member.Token, map[string]any{
		"client_live_session_id": "clive_scope_b_after_room_a_headphones_mute",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)
	participant = response["participant"].(map[string]any)
	if participant["mic_blocked"] != false || participant["voice_blocked"] != false ||
		participant["headphones_blocked"] != false || participant["headphones_muted"] != false {
		t.Fatalf("room A headphones mute should not affect room B: %v", participant)
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
