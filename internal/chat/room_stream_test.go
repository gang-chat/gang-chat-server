package chat

import (
	"net/http"
	"testing"
	"time"

	"github.com/zhuangkaiyi/gang-chat/server/internal/eventbus"
)

// streamClient mimics one device's SSE connection: a bus subscription whose
// room interest is seeded the way liveStream seeds it at connect time.
type streamClient struct {
	t   *testing.T
	sub *eventbus.Subscription
}

func (h *apiHarness) connectStream(userID string) *streamClient {
	h.t.Helper()
	sub := h.bus.Subscribe(userID)
	rooms, err := (&Handler{DB: h.db}).userRoomIDs(userID)
	if err != nil {
		h.t.Fatalf("seed rooms: %v", err)
	}
	sub.SetRooms(rooms)
	h.t.Cleanup(sub.Close)
	return &streamClient{t: h.t, sub: sub}
}

// await drains events until one with the wanted type arrives, returning its
// payload. Other event types are skipped (a single change can fan out several
// events). Fails if none arrives in time.
func (s *streamClient) await(eventType string) map[string]any {
	s.t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case ev, ok := <-s.sub.Events():
			if !ok {
				s.t.Fatalf("stream closed before %q", eventType)
			}
			if ev.Type != eventType {
				continue
			}
			data, _ := ev.Data.(roomSnapshot)
			return map[string]any{"room_id": ev.RoomID, "snapshot": data}
		case <-deadline:
			s.t.Fatalf("timed out waiting for %q", eventType)
			return nil
		}
	}
}

// expectSilent asserts no event arrives within a short window.
func (s *streamClient) expectSilent() {
	s.t.Helper()
	select {
	case ev := <-s.sub.Events():
		s.t.Fatalf("expected no event, got %q (room %s)", ev.Type, ev.RoomID)
	case <-time.After(75 * time.Millisecond):
	}
}

// nextType returns the type of the next event, or "" if none arrives.
func (s *streamClient) nextType() string {
	s.t.Helper()
	select {
	case ev, ok := <-s.sub.Events():
		if !ok {
			return ""
		}
		return ev.Type
	case <-time.After(time.Second):
		s.t.Fatalf("timed out waiting for any event")
		return ""
	}
}

func TestStreamApprovalAddsRoomForApplicant(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_approve")
	applicant := api.register("applicant_approve")

	room := api.createRoom(owner.Token, map[string]any{"name": "Gated", "join_policy": "approval_required"})
	roomID := room["id"].(string)

	// Both connect their streams now. The applicant isn't a member yet, so
	// their connection has no interest in this room — the whole point of the
	// per-user delivery path.
	ownerStream := api.connectStream(owner.User["id"].(string))
	applicantStream := api.connectStream(applicant.User["id"].(string))

	// Applicant requests to join (202 — pending), then owner approves.
	status, resp := api.request(http.MethodPost, "/rooms/"+roomID+"/join", applicant.Token, nil)
	api.requireStatus(status, http.StatusAccepted, resp)
	requestID := resp["join_request"].(map[string]any)["id"].(string)

	status, resp = api.request(http.MethodPatch, "/rooms/"+roomID+"/join-requests/"+requestID, owner.Token, map[string]any{"decision": "approve"})
	api.requireStatus(status, http.StatusOK, resp)

	// Applicant learns they joined via room_added even though their connection
	// never subscribed to this room.
	added := applicantStream.await("room_added")
	if added["room_id"] != roomID {
		t.Fatalf("room_added for wrong room: %v", added)
	}
	snap := added["snapshot"].(roomSnapshot)
	if snap.MemberCount != 2 {
		t.Fatalf("expected member_count 2, got %d", snap.MemberCount)
	}

	// Owner (already a member) gets room_updated with the bumped count.
	updated := ownerStream.await("room_updated")
	if updated["snapshot"].(roomSnapshot).MemberCount != 2 {
		t.Fatalf("owner update should show 2 members: %v", updated)
	}
}

func TestStreamRejectionIsSilent(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_reject")
	applicant := api.register("applicant_reject")

	room := api.createRoom(owner.Token, map[string]any{"name": "Gated2", "join_policy": "approval_required"})
	roomID := room["id"].(string)

	applicantStream := api.connectStream(applicant.User["id"].(string))

	status, resp := api.request(http.MethodPost, "/rooms/"+roomID+"/join", applicant.Token, nil)
	api.requireStatus(status, http.StatusAccepted, resp)
	requestID := resp["join_request"].(map[string]any)["id"].(string)

	status, resp = api.request(http.MethodPatch, "/rooms/"+roomID+"/join-requests/"+requestID, owner.Token, map[string]any{"decision": "reject"})
	api.requireStatus(status, http.StatusOK, resp)

	// A rejected applicant gains no membership, so nothing should reach them.
	applicantStream.expectSilent()
}

func TestStreamLeaveNotifiesBothSides(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_leave")
	joiner := api.register("joiner_leave")

	room := api.createRoom(owner.Token, map[string]any{"name": "Open", "join_policy": "open"})
	roomID := room["id"].(string)

	status, resp := api.request(http.MethodPost, "/rooms/"+roomID+"/join", joiner.Token, nil)
	api.requireStatus(status, http.StatusOK, resp)

	ownerStream := api.connectStream(owner.User["id"].(string))
	joinerStream := api.connectStream(joiner.User["id"].(string))

	status, resp = api.request(http.MethodPost, "/rooms/"+roomID+"/leave", joiner.Token, nil)
	api.requireStatus(status, http.StatusOK, resp)

	// Leaver drops the room; owner sees the smaller member_count.
	deleted := joinerStream.await("room_deleted")
	if deleted["room_id"] != roomID {
		t.Fatalf("room_deleted for wrong room: %v", deleted)
	}
	updated := ownerStream.await("room_updated")
	if updated["snapshot"].(roomSnapshot).MemberCount != 1 {
		t.Fatalf("owner should see 1 member after leave: %v", updated)
	}
}

func TestStreamRemoveMemberNotifiesBothSides(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_remove")
	member := api.register("member_remove")

	room := api.createRoom(owner.Token, map[string]any{"name": "Open2", "join_policy": "open"})
	roomID := room["id"].(string)

	status, resp := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, resp)

	ownerStream := api.connectStream(owner.User["id"].(string))
	memberStream := api.connectStream(member.User["id"].(string))

	status, resp = api.request(http.MethodDelete, "/rooms/"+roomID+"/members/"+member.User["id"].(string), owner.Token, nil)
	api.requireStatus(status, http.StatusOK, resp)

	if got := memberStream.await("room_deleted"); got["room_id"] != roomID {
		t.Fatalf("removed member should get room_deleted: %v", got)
	}
	if got := ownerStream.await("room_updated"); got["snapshot"].(roomSnapshot).MemberCount != 1 {
		t.Fatalf("owner should see 1 member after removal: %v", got)
	}
}

func TestStreamSettingsChangeUpdatesMembers(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_settings")
	roomCard := api.createRoom(owner.Token, map[string]any{"name": "Before", "join_policy": "open"})
	roomID := roomCard["id"].(string)

	ownerStream := api.connectStream(owner.User["id"].(string))

	status, resp := api.request(http.MethodPatch, "/rooms/"+roomID+"/settings", owner.Token, map[string]any{"name": "After"})
	api.requireStatus(status, http.StatusOK, resp)

	updated := ownerStream.await("room_updated")
	if updated["snapshot"].(roomSnapshot).Name != "After" {
		t.Fatalf("settings update should carry new name: %v", updated)
	}
}

func TestStreamMessageRefreshesLastMessage(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_msg")
	roomCard := api.createRoom(owner.Token, map[string]any{"name": "Chatty", "join_policy": "open"})
	roomID := roomCard["id"].(string)

	ownerStream := api.connectStream(owner.User["id"].(string))
	_ = api.sendMessage(owner.Token, roomID, "hello world")

	updated := ownerStream.await("room_updated")
	snap := updated["snapshot"].(roomSnapshot)
	if snap.LastMessage == nil || snap.LastMessage.BodyPreview != "hello world" {
		t.Fatalf("room_updated should carry the new last_message: %+v", snap.LastMessage)
	}
}

func TestStreamDeleteRoomNotifiesMembers(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_delete")
	member := api.register("member_delete")

	room := api.createRoom(owner.Token, map[string]any{"name": "Doomed", "join_policy": "open"})
	roomID := room["id"].(string)
	status, resp := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, resp)

	memberStream := api.connectStream(member.User["id"].(string))

	status, resp = api.request(http.MethodDelete, "/rooms/"+roomID, owner.Token, nil)
	api.requireStatus(status, http.StatusOK, resp)

	if got := memberStream.await("room_deleted"); got["room_id"] != roomID {
		t.Fatalf("member should get room_deleted on room delete: %v", got)
	}
}

func TestStreamCreateRoomAddsForCreator(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_create")
	ownerStream := api.connectStream(owner.User["id"].(string))

	_ = api.createRoom(owner.Token, map[string]any{"name": "Fresh", "join_policy": "open"})

	if got := ownerStream.nextType(); got != "room_added" {
		t.Fatalf("creator should get room_added, got %q", got)
	}
}
