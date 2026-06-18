package chat

import (
	"net/http"
	"testing"
)

// TestSuperuserLiveGhost verifies the "ghost" model: a superuser can enter any
// room's voice channel and post messages without ever becoming a room member,
// so they stay out of the member roster while being visible in live.
func TestSuperuserLiveGhost(t *testing.T) {
	api := newAPIHarness(t)
	super := api.login("GANG", "64n9-Ch47")
	superID := super.User["id"].(string)

	owner := api.register("ghost_owner")
	room := api.createRoom(owner.Token, map[string]any{
		"name":        "Closed Ops",
		"visibility":  "private",
		"join_policy": "closed",
	})
	roomID := room["id"].(string)

	// The superuser never joins the room. They drop straight into live.
	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/live/join", super.Token, map[string]any{
		"client_live_session_id": "clive_super_ghost",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)
	if _, ok := response["livekit"].(map[string]any); !ok {
		t.Fatalf("superuser live join should return a livekit token: %v", response)
	}

	// They are NOT in the member roster, and member_count is unchanged (owner only).
	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/members", super.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	members := response["members"].([]any)
	if len(members) != 1 {
		t.Fatalf("superuser must not appear in member list, got %d members: %v", len(members), response)
	}
	for _, item := range members {
		m := item.(map[string]any)
		if m["user"].(map[string]any)["id"] == superID {
			t.Fatalf("superuser leaked into member list: %v", response)
		}
	}

	// But they ARE a live participant.
	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/live", super.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	live := response["live"].(map[string]any)
	if got := int(live["participant_count"].(float64)); got != 1 {
		t.Fatalf("superuser should be the sole live participant, got count %d: %v", got, response)
	}
	participantByUserID(t, live, superID) // fatals if absent

	// They can update their own live state without a membership row.
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me", super.Token, map[string]any{
		"mic_muted": false,
	})
	api.requireStatus(status, http.StatusOK, response)

	// They can post a message; it carries their normal GANG sender identity.
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/messages", super.Token, map[string]any{
		"client_message_id": "msg_super_ghost",
		"type":              "text",
		"body":              "moderation notice",
	})
	api.requireStatus(status, http.StatusCreated, response)
	sender := response["message"].(map[string]any)["sender"].(map[string]any)
	if sender["id"] != superID {
		t.Fatalf("message should be attributed to the superuser, got: %v", sender)
	}
	if sender["is_superuser"] != true || sender["room_role"] != "superuser" {
		t.Fatalf("message sender should expose superuser identity: %v", sender)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/messages?limit=20", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	messages := response["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("expected one listed message, got %d: %v", len(messages), response)
	}
	listedSender := messages[0].(map[string]any)["sender"].(map[string]any)
	if listedSender["is_superuser"] != true || listedSender["room_role"] != "superuser" {
		t.Fatalf("listed message sender should expose superuser identity: %v", listedSender)
	}

	// Joining live still must not have created a membership row.
	var memberships int
	_ = api.db.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, superID).Scan(&memberships)
	if memberships != 0 {
		t.Fatalf("superuser ghost must hold no membership row, found %d", memberships)
	}
}

// TestNonMemberStillBlockedFromLiveAndSend confirms relaxing the gates to
// requireRoomAccess only opened the door for the superuser: an ordinary
// non-member still gets 404 on both live join and message send.
func TestNonMemberStillBlockedFromLiveAndSend(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("gate_owner")
	outsider := api.register("gate_outsider")
	room := api.createRoom(owner.Token, map[string]any{
		"name":        "Members Only",
		"visibility":  "private",
		"join_policy": "closed",
	})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/live/join", outsider.Token, map[string]any{
		"client_live_session_id": "clive_outsider",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusNotFound, response)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/messages", outsider.Token, map[string]any{
		"client_message_id": "msg_outsider",
		"type":              "text",
		"body":              "let me in",
	})
	api.requireStatus(status, http.StatusNotFound, response)
}
