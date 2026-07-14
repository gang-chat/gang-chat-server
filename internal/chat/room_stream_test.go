package chat

import (
	"net/http"
	"sort"
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
			payload := map[string]any{"room_id": ev.RoomID}
			if data, ok := ev.Data.(roomSnapshot); ok {
				payload["snapshot"] = data
			} else if data, ok := ev.Data.(map[string]any); ok {
				for key, value := range data {
					payload[key] = value
				}
			}
			return payload
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
	// Creating the pending request changes the owner's room card before the
	// approval changes membership. Consume and verify that distinct snapshot so
	// the assertion below observes the approval event, not this earlier update.
	pending := ownerStream.await("room_updated")
	if pending["snapshot"].(roomSnapshot).MemberCount != 1 {
		t.Fatalf("pending request must not change member count: %v", pending)
	}

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

func TestStreamRejectionNotifiesApplicantApplications(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_reject")
	applicant := api.register("applicant_reject")

	room := api.createRoom(owner.Token, map[string]any{"name": "Gated2", "join_policy": "approval_required"})
	roomID := room["id"].(string)

	applicantStream := api.connectStream(applicant.User["id"].(string))

	status, resp := api.request(http.MethodPost, "/rooms/"+roomID+"/join", applicant.Token, nil)
	api.requireStatus(status, http.StatusAccepted, resp)
	requestID := resp["join_request"].(map[string]any)["id"].(string)
	applicantStream.await("room_applications_updated")

	status, resp = api.request(http.MethodPatch, "/rooms/"+roomID+"/join-requests/"+requestID, owner.Token, map[string]any{"decision": "reject"})
	api.requireStatus(status, http.StatusOK, resp)

	// A rejected applicant gains no membership, but their notification panel
	// should refresh so it can show the rejected application.
	applicantStream.await("room_applications_updated")
	applicantStream.expectSilent()
}

func TestStreamInviteAcceptanceRefreshesPendingJoinRequests(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_invite_approve")
	applicant := api.register("applicant_invite_approve")

	room := api.createRoom(owner.Token, map[string]any{"name": "Invite Approval", "join_policy": "approval_required"})
	roomID := room["id"].(string)

	ownerStream := api.connectStream(owner.User["id"].(string))

	status, resp := api.request(http.MethodPost, "/rooms/"+roomID+"/join", applicant.Token, map[string]any{
		"reason": "Applying first",
	})
	api.requireStatus(status, http.StatusAccepted, resp)
	created := ownerStream.await("room_join_requests_updated")
	if created["room_id"] != roomID {
		t.Fatalf("join request update for wrong room: %v", created)
	}

	status, resp = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": applicant.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, resp)
	inviteID := resp["invite"].(map[string]any)["id"].(string)

	status, resp = api.request(http.MethodPatch, "/room-invites/"+inviteID, applicant.Token, map[string]any{"decision": "accept"})
	api.requireStatus(status, http.StatusOK, resp)

	updated := ownerStream.await("room_join_requests_updated")
	if updated["room_id"] != roomID {
		t.Fatalf("join request update for wrong room after invite acceptance: %v", updated)
	}
	status, resp = api.request(http.MethodGet, "/rooms/"+roomID+"/join-requests?status=pending", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, resp)
	if got := len(resp["requests"].([]any)); got != 0 {
		t.Fatalf("invite acceptance should clear pending join request, got %d: %v", got, resp)
	}
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

func TestStreamJoinPolicyChangeRefreshesPendingInvites(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_invite_policy_stream")
	target := api.register("target_invite_policy_stream")
	room := api.createRoom(owner.Token, map[string]any{"name": "Invite Stream", "join_policy": "approval_required"})
	roomID := room["id"].(string)

	status, resp := api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": target.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, resp)

	targetStream := api.connectStream(target.User["id"].(string))

	status, resp = api.request(http.MethodPatch, "/rooms/"+roomID+"/settings", owner.Token, map[string]any{"join_policy": "closed"})
	api.requireStatus(status, http.StatusOK, resp)
	targetStream.await("room_invites_updated")

	status, resp = api.request(http.MethodPatch, "/rooms/"+roomID+"/settings", owner.Token, map[string]any{"join_policy": "approval_required"})
	api.requireStatus(status, http.StatusOK, resp)
	targetStream.await("room_invites_updated")
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
	updated = ownerStream.await("room_updated")
	snap = updated["snapshot"].(roomSnapshot)
	if snap.LastMessage == nil || snap.LastMessage.BodyPreview != `[语音] 15"` {
		t.Fatalf("room_updated should label voice last_message: %+v", snap.LastMessage)
	}
}

func TestMarkReadIsMonotonicAndSyncsEveryDevice(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("read_sync_owner")
	member := api.register("read_sync_member")
	roomCard := api.createRoom(owner.Token, map[string]any{
		"name":        "Read Sync",
		"join_policy": "open",
	})
	roomID := roomCard["id"].(string)
	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	first := api.sendMessage(owner.Token, roomID, "first")
	second := api.sendMessage(owner.Token, roomID, "second")
	messageIDs := []string{first["id"].(string), second["id"].(string)}
	sort.Strings(messageIDs)
	olderID, newerID := messageIDs[0], messageIDs[1]
	// Exercise the deterministic id tie-breaker used when messages land in the
	// same millisecond. The lexicographically larger id is later in the list.
	sharedCreatedAt := nowMillis() - 1000
	if _, err := api.db.Exec(
		`UPDATE messages SET created_at = ? WHERE id IN (?, ?)`,
		sharedCreatedAt,
		olderID,
		newerID,
	); err != nil {
		t.Fatalf("align message timestamps: %v", err)
	}

	deviceOne := api.connectStream(member.User["id"].(string))
	deviceTwo := api.connectStream(member.User["id"].(string))

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/read", member.Token, map[string]any{
		"last_read_message_id": newerID,
	})
	api.requireStatus(status, http.StatusOK, response)
	if response["unread_count"] != float64(0) {
		t.Fatalf("newest cursor should clear unread messages: %v", response)
	}
	if got := deviceOne.await("room_updated")["snapshot"].(roomSnapshot).UnreadCount; got != 0 {
		t.Fatalf("device one should receive the cleared count, got %d", got)
	}
	if got := deviceTwo.await("room_updated")["snapshot"].(roomSnapshot).UnreadCount; got != 0 {
		t.Fatalf("device two should receive the cleared count, got %d", got)
	}

	// A stale device must not move the account cursor backwards.
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/read", member.Token, map[string]any{
		"last_read_message_id": olderID,
	})
	api.requireStatus(status, http.StatusOK, response)
	if response["unread_count"] != float64(0) {
		t.Fatalf("stale cursor should be ignored: %v", response)
	}
	if got := deviceOne.await("room_updated")["snapshot"].(roomSnapshot).UnreadCount; got != 0 {
		t.Fatalf("device one should retain the committed cursor, got %d", got)
	}
	if got := deviceTwo.await("room_updated")["snapshot"].(roomSnapshot).UnreadCount; got != 0 {
		t.Fatalf("device two should retain the committed cursor, got %d", got)
	}
	var storedCursor string
	if err := api.db.QueryRow(
		`SELECT last_read_message_id FROM room_reads WHERE room_id = ? AND user_id = ?`,
		roomID,
		member.User["id"].(string),
	).Scan(&storedCursor); err != nil {
		t.Fatalf("read stored cursor: %v", err)
	}
	if storedCursor != newerID {
		t.Fatalf("stale device regressed cursor: got %q want %q", storedCursor, newerID)
	}
	freshDevice := api.login("read_sync_member", "correct horse battery staple")
	status, response = api.request(http.MethodGet, "/rooms", freshDevice.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := roomCardByID(t, response, roomID)["unread_count"]; got != float64(0) {
		t.Fatalf("fresh login should preserve the committed read state, got %v", got)
	}

	_ = api.sendMessage(owner.Token, roomID, "third")
	if got := deviceOne.await("room_updated")["snapshot"].(roomSnapshot).UnreadCount; got != 1 {
		t.Fatalf("device one should see exactly one new message, got %d", got)
	}
	if got := deviceTwo.await("room_updated")["snapshot"].(roomSnapshot).UnreadCount; got != 1 {
		t.Fatalf("device two should see exactly one new message, got %d", got)
	}
	status, response = api.request(http.MethodGet, "/rooms", freshDevice.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := roomCardByID(t, response, roomID)["unread_count"]; got != float64(1) {
		t.Fatalf("fresh login snapshot should retain one unread message, got %v", got)
	}
}

func TestStreamRoomProfileChangesCarryAccurateUnreadCount(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_profile_stream")
	member := api.register("member_profile_stream")
	roomCard := api.createRoom(owner.Token, map[string]any{
		"name":        "Before Name",
		"description": "Before bio",
		"join_policy": "open",
	})
	roomID := roomCard["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	messages := listRoomMessages(t, api, member.Token, roomID)
	if len(messages) == 0 {
		t.Fatalf("expected join system message before mark-read")
	}
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/read", member.Token, map[string]any{
		"last_read_message_id": messages[len(messages)-1]["id"],
	})
	api.requireStatus(status, http.StatusOK, response)
	time.Sleep(time.Millisecond)

	memberStream := api.connectStream(member.User["id"].(string))
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID, owner.Token, map[string]any{
		"name":        "After Name",
		"description": "After bio",
	})
	api.requireStatus(status, http.StatusOK, response)

	updated := memberStream.await("room_updated")
	snap := updated["snapshot"].(roomSnapshot)
	if snap.UnreadCount != 2 {
		t.Fatalf("room_updated should carry both profile-change system messages as unread: %+v", snap)
	}
	if snap.LastMessage == nil || snap.LastMessage.SenderDisplayName != "" || snap.LastMessage.BodyPreview != "房间简介 被 owner_profile_stream 修改为 After bio" {
		t.Fatalf("room_updated should carry chat-aligned profile-change preview: %+v", snap.LastMessage)
	}
}

func TestStreamRoomMemberProfileChangedReloadsMembersPanel(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_member_profile_changed")
	member := api.register("member_profile_changed")
	roomCard := api.createRoom(owner.Token, map[string]any{"name": "Member Profile Changed", "join_policy": "open"})
	roomID := roomCard["id"].(string)
	memberID := member.User["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	ownerStream := api.connectStream(owner.User["id"].(string))
	memberStream := api.connectStream(memberID)
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+memberID, owner.Token, map[string]any{
		"room_display_name": "Changed In Room",
	})
	api.requireStatus(status, http.StatusOK, response)

	for label, stream := range map[string]*streamClient{"owner": ownerStream, "member": memberStream} {
		updated := stream.await("room_member_profile_changed")
		if updated["room_id"] != roomID || updated["user_id"] != memberID {
			t.Fatalf("%s should receive changed member profile payload: %v", label, updated)
		}
	}
}

func TestStreamLiveLeaveRefreshesRoomSnapshotWithoutSystemMessage(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_live_left")
	member := api.register("member_live_left")
	roomCard := api.createRoom(owner.Token, map[string]any{"name": "Live Left", "join_policy": "open"})
	roomID := roomCard["id"].(string)
	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	ownerStream := api.connectStream(owner.User["id"].(string))
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/live/join", member.Token, map[string]any{
		"client_live_session_id": "left_stream_member",
		"source":                 "live_panel",
	})
	api.requireStatus(status, http.StatusOK, response)
	updated := ownerStream.await("room_updated")
	snap := updated["snapshot"].(roomSnapshot)
	if snap.LiveParticipantCount != 1 {
		t.Fatalf("live join should update room live count: %+v", snap)
	}
	if snap.LastMessage == nil || snap.LastMessage.BodyPreview != "加入了房间" {
		t.Fatalf("live join should not replace last_message with a system message: %+v", snap.LastMessage)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/live/me", member.Token, map[string]any{
		"connection_state": "left",
	})
	api.requireStatus(status, http.StatusOK, response)
	updated = ownerStream.await("room_updated")
	snap = updated["snapshot"].(roomSnapshot)
	if snap.LastMessage == nil || snap.LastMessage.BodyPreview != "加入了房间" {
		t.Fatalf("live leave should not replace last_message with a system message: %+v", snap.LastMessage)
	}
	if snap.LiveParticipantCount != 0 {
		t.Fatalf("left live participants should not count in room snapshot: %+v", snap)
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

	status, resp = api.request(http.MethodDelete, "/rooms/"+roomID, owner.Token, map[string]any{"confirm_name": "Doomed"})
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
