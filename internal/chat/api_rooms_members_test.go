package chat

import (
	"fmt"
	"github.com/zhuangkaiyi/gang-chat/server/internal/idgen"
	"net/http"
	"testing"
)

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

	if _, err := api.db.Exec(
		`UPDATE users
		    SET bio = ?, gender = ?, email_public = 1,
		        phone_number = ?, phone_number_normalized = ?, phone_number_public = 1
		  WHERE id = ?`,
		"Ships quietly", "female", "+8613800000000", "8613800000000", alice.User["id"].(string),
	); err != nil {
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
	if user["gender"] != "female" {
		t.Fatalf("profile should include gender: %v", profile)
	}
	if user["is_online"] != true {
		t.Fatalf("profile should include online state: %v", profile)
	}
	if _, ok := user["email"]; ok {
		t.Fatalf("viewer without public email should not see public email: %v", user)
	}
	if _, ok := user["email_public"]; ok {
		t.Fatalf("viewer without public email should not see email visibility: %v", user)
	}
	if _, ok := user["phone_number"]; ok {
		t.Fatalf("viewer without public phone should not see public phone: %v", user)
	}
	if _, ok := user["phone_number_public"]; ok {
		t.Fatalf("viewer without public phone should not see phone visibility: %v", user)
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
	if user["bio"] != "Ships quietly" || user["gender"] != "female" || user["is_online"] != true {
		t.Fatalf("global profile should include latest user state: %v", user)
	}
	commonRooms = user["common_rooms"].([]any)
	if len(commonRooms) != 1 || commonRooms[0].(map[string]any)["id"] != room1ID {
		t.Fatalf("global profile should include viewer-visible common rooms: %v", commonRooms)
	}

	if _, err := api.db.Exec(`UPDATE users SET email_public = 1 WHERE id = ?`, viewer.User["id"].(string)); err != nil {
		t.Fatalf("publish viewer email: %v", err)
	}
	status, response = api.request(http.MethodGet, "/users/"+alice.User["id"].(string)+"/profile", viewer.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	user = response["profile"].(map[string]any)["user"].(map[string]any)
	if user["email"] != "profile_alice@example.com" || user["email_public"] != true {
		t.Fatalf("viewer with public email should see target public email: %v", user)
	}
	if _, ok := user["phone_number"]; ok {
		t.Fatalf("viewer without bound public phone should not see public phone: %v", user)
	}
	if _, ok := user["phone_number_public"]; ok {
		t.Fatalf("viewer without bound public phone should not see phone visibility: %v", user)
	}

	if _, err := api.db.Exec(
		`UPDATE users SET phone_number = ?, phone_number_normalized = ?, phone_number_public = 1 WHERE id = ?`,
		"+8613900000000", "8613900000000", viewer.User["id"].(string),
	); err != nil {
		t.Fatalf("publish viewer phone: %v", err)
	}
	status, response = api.request(http.MethodGet, "/rooms/"+room1ID+"/members/"+alice.User["id"].(string)+"/profile", viewer.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	user = response["profile"].(map[string]any)["user"].(map[string]any)
	if user["phone_number"] != "+8613800000000" || user["phone_number_public"] != true {
		t.Fatalf("viewer with public phone should see target public phone: %v", user)
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
	status, response = api.request(http.MethodGet, "/rooms", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	var listed map[string]any
	for _, candidate := range response["rooms"].([]any) {
		roomCard := candidate.(map[string]any)
		if roomCard["id"] == roomID {
			listed = roomCard
			break
		}
	}
	if listed == nil || listed["ai_voice_announcements_enabled"] != false {
		t.Fatalf("room card should expose the AI voice announcement switch: %v", response)
	}

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
	messages := listRoomMessages(t, api, member.Token, roomID)
	nameMessage := requireSystemMessage(t, messages, systemEventRoomNameChanged, owner.User["id"].(string))
	if nameMessage["body"] != "房间名称修改为Managed" {
		t.Fatalf("room name system body should omit actor: %v", nameMessage)
	}
	nameAttachment := systemAttachment(t, nameMessage)
	if nameAttachment["old_value"] != "Manage Me" || nameAttachment["new_value"] != "Managed" {
		t.Fatalf("room name system attachment mismatch: %v", nameAttachment)
	}
	if actor := nameAttachment["actor"].(map[string]any); actor["id"] != owner.User["id"] {
		t.Fatalf("room name system message should include actor: %v", nameAttachment)
	}
	descriptionMessage := requireSystemMessage(t, messages, systemEventRoomBioChanged, owner.User["id"].(string))
	if descriptionMessage["body"] != "房间简介修改为\nRoom bio" {
		t.Fatalf("room description system body should omit actor: %v", descriptionMessage)
	}
	descriptionAttachment := systemAttachment(t, descriptionMessage)
	if descriptionAttachment["old_value"] != "old intro" || descriptionAttachment["new_value"] != "Room bio" {
		t.Fatalf("room description system attachment mismatch: %v", descriptionAttachment)
	}
	if actor := descriptionAttachment["actor"].(map[string]any); actor["id"] != owner.User["id"] {
		t.Fatalf("room description system message should include actor: %v", descriptionAttachment)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/me", member.Token, map[string]any{
		"remark_name":         "My Managed Room",
		"room_display_name":   "Local Nick",
		"default_avatar_key":  "red-4",
		"notification_policy": "silent",
		"is_pinned":           true,
	})
	api.requireStatus(status, http.StatusOK, response)
	personalRoom := response["room"].(map[string]any)
	profile := personalRoom["personal_profile"].(map[string]any)
	if personalRoom["remark_name"] != "My Managed Room" || personalRoom["notification_policy"] != "silent" || personalRoom["is_pinned"] != true {
		t.Fatalf("room personal fields not returned on detail: %v", personalRoom)
	}
	if profile["display_name"] != "Local Nick" {
		t.Fatalf("room personal profile not returned: %v", profile)
	}
	if _, ok := profile["avatar_url"]; ok {
		t.Fatalf("room personal profile should not expose avatar_url: %v", profile)
	}
	if _, ok := profile["default_avatar_key"]; ok {
		t.Fatalf("room personal profile should not expose default_avatar_key: %v", profile)
	}
	settings := response["settings"].(map[string]any)
	if settings["notification_policy"] != "silent" || settings["is_pinned"] != true {
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

func TestAdminsCanEditLowerMemberRoomDisplayName(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("display_name_owner")
	admin := api.register("display_name_admin")
	peerAdmin := api.register("display_name_peer_admin")
	member := api.register("display_name_member")

	room := api.createRoom(owner.Token, map[string]any{"name": "Display Name Gate", "join_policy": "open"})
	roomID := room["id"].(string)
	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", admin.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", peerAdmin.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)

	adminID := admin.User["id"].(string)
	peerAdminID := peerAdmin.User["id"].(string)
	memberID := member.User["id"].(string)
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+adminID, owner.Token, map[string]any{"role": "admin"})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+peerAdminID, owner.Token, map[string]any{"role": "admin"})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+memberID, admin.Token, map[string]any{
		"room_display_name": "Room Member",
	})
	api.requireStatus(status, http.StatusOK, response)
	updated := response["member"].(map[string]any)
	if updated["room_display_name"] != "Room Member" {
		t.Fatalf("member payload should include room_display_name: %v", updated)
	}
	user := updated["user"].(map[string]any)
	if user["room_display_name"] != "Room Member" {
		t.Fatalf("member user payload should include room_display_name: %v", updated)
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/members?limit=50", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	listed := roomMemberByID(t, response, memberID)
	listedUser := listed["user"].(map[string]any)
	if listedUser["room_display_name"] != "Room Member" {
		t.Fatalf("member list should include updated room_display_name: %v", listed)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+peerAdminID, admin.Token, map[string]any{
		"room_display_name": "Peer Alias",
	})
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+adminID, owner.Token, map[string]any{
		"room_display_name": "",
	})
	api.requireStatus(status, http.StatusOK, response)
	updated = response["member"].(map[string]any)
	if updated["room_display_name"] != nil {
		t.Fatalf("empty room_display_name should clear alias: %v", updated)
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

func TestRoomCardsExposePendingJoinRequestBadgeForAdmins(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("pending_badge_owner")
	admin := api.register("pending_badge_admin")
	member := api.register("pending_badge_member")
	applicant := api.register("pending_badge_applicant")

	room := api.createRoom(owner.Token, map[string]any{
		"name":        "Pending Badge",
		"join_policy": "open",
	})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", admin.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+admin.User["id"].(string), owner.Token, map[string]any{
		"role": "admin",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/settings", owner.Token, map[string]any{
		"join_policy": "approval_required",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", applicant.Token, map[string]any{"reason": "let me in"})
	api.requireStatus(status, http.StatusAccepted, response)
	requestID := response["join_request"].(map[string]any)["id"].(string)

	status, response = api.request(http.MethodGet, "/rooms", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := roomCardByID(t, response, roomID)["has_pending_join_requests"]; got != true {
		t.Fatalf("owner room card should expose pending join request, got %v in %v", got, response)
	}
	status, response = api.request(http.MethodGet, "/rooms", admin.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := roomCardByID(t, response, roomID)["has_pending_join_requests"]; got != true {
		t.Fatalf("admin room card should expose pending join request, got %v in %v", got, response)
	}
	status, response = api.request(http.MethodGet, "/rooms", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := roomCardByID(t, response, roomID)["has_pending_join_requests"]; got != false {
		t.Fatalf("member room card should not expose pending join request, got %v in %v", got, response)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/join-requests/"+requestID, owner.Token, map[string]any{
		"decision": "reject",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/rooms", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := roomCardByID(t, response, roomID)["has_pending_join_requests"]; got != false {
		t.Fatalf("resolved join request should clear room card badge, got %v in %v", got, response)
	}
}

func TestRoomJoinPolicyChangeAutoReviewsPendingRequests(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("policy_review_owner")
	approved := api.register("policy_review_approved")
	rejected := api.register("policy_review_rejected")
	ownerID := owner.User["id"].(string)
	approvedID := approved.User["id"].(string)
	rejectedID := rejected.User["id"].(string)

	openRoom := api.createRoom(owner.Token, map[string]any{
		"name":        "Auto Approve",
		"join_policy": "approval_required",
	})
	openRoomID := openRoom["id"].(string)
	status, response := api.request(http.MethodPost, "/rooms/"+openRoomID+"/join", approved.Token, map[string]any{"reason": "approve me"})
	api.requireStatus(status, http.StatusAccepted, response)

	status, response = api.request(http.MethodPatch, "/rooms/"+openRoomID+"/settings", owner.Token, map[string]any{
		"join_policy": "open",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/rooms/"+openRoomID+"/join-requests?status=pending", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if requests := response["requests"].([]any); len(requests) != 0 {
		t.Fatalf("opening room should auto-approve pending requests: %v", response)
	}
	var approvedStatus, approvedReviewer string
	if err := api.db.QueryRow(
		`SELECT status, reviewer_user_id FROM join_requests WHERE room_id = ? AND user_id = ?`,
		openRoomID,
		approvedID,
	).Scan(&approvedStatus, &approvedReviewer); err != nil {
		t.Fatalf("read approved join request: %v", err)
	}
	if approvedStatus != "approved" || approvedReviewer != ownerID {
		t.Fatalf("join request should be approved by owner, status=%s reviewer=%s", approvedStatus, approvedReviewer)
	}
	status, response = api.request(http.MethodGet, "/rooms/"+openRoomID, approved.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if response["room"].(map[string]any)["my_membership"].(map[string]any)["role"] != "member" {
		t.Fatalf("auto-approved user should become a member: %v", response)
	}
	messages := listRoomMessages(t, api, owner.Token, openRoomID)
	joinPolicyMessage := requireSystemMessage(t, messages, systemEventRoomJoinPolicyChanged, ownerID)
	joinPolicyAttachment := systemAttachment(t, joinPolicyMessage)
	if joinPolicyAttachment["old_value"] != "approval_required" || joinPolicyAttachment["new_value"] != "open" {
		t.Fatalf("join policy system attachment mismatch: %v", joinPolicyAttachment)
	}
	if !hasSystemMessage(t, messages, systemEventRoomMemberJoined, approvedID) {
		t.Fatalf("auto-approved user should have a member joined system message: %v", messages)
	}

	closedRoom := api.createRoom(owner.Token, map[string]any{
		"name":        "Auto Reject",
		"join_policy": "approval_required",
	})
	closedRoomID := closedRoom["id"].(string)
	status, response = api.request(http.MethodPost, "/rooms/"+closedRoomID+"/join", rejected.Token, map[string]any{"reason": "reject me"})
	api.requireStatus(status, http.StatusAccepted, response)

	status, response = api.request(http.MethodPatch, "/rooms/"+closedRoomID+"/settings", owner.Token, map[string]any{
		"join_policy": "closed",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/rooms/"+closedRoomID+"/join-requests?status=pending", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if requests := response["requests"].([]any); len(requests) != 0 {
		t.Fatalf("closing room should auto-reject pending requests: %v", response)
	}
	var rejectedStatus, rejectedReviewer string
	if err := api.db.QueryRow(
		`SELECT status, reviewer_user_id FROM join_requests WHERE room_id = ? AND user_id = ?`,
		closedRoomID,
		rejectedID,
	).Scan(&rejectedStatus, &rejectedReviewer); err != nil {
		t.Fatalf("read rejected join request: %v", err)
	}
	if rejectedStatus != "rejected" || rejectedReviewer != ownerID {
		t.Fatalf("join request should be rejected by owner, status=%s reviewer=%s", rejectedStatus, rejectedReviewer)
	}
	var membershipCount int
	if err := api.db.QueryRow(
		`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`,
		closedRoomID,
		rejectedID,
	).Scan(&membershipCount); err != nil {
		t.Fatalf("count rejected membership: %v", err)
	}
	if membershipCount != 0 {
		t.Fatalf("auto-rejected user should not become a member")
	}
	messages = listRoomMessages(t, api, owner.Token, closedRoomID)
	joinPolicyMessage = requireSystemMessage(t, messages, systemEventRoomJoinPolicyChanged, ownerID)
	joinPolicyAttachment = systemAttachment(t, joinPolicyMessage)
	if joinPolicyAttachment["old_value"] != "approval_required" || joinPolicyAttachment["new_value"] != "closed" {
		t.Fatalf("closed join policy system attachment mismatch: %v", joinPolicyAttachment)
	}
	if hasSystemMessage(t, messages, systemEventRoomMemberJoined, rejectedID) {
		t.Fatalf("auto-rejected user should not have a member joined system message: %v", messages)
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

func TestRoomBlacklistFlow(t *testing.T) {
	api := newAPIHarness(t)
	super := api.login("GANG", "64n9-Ch47")
	owner := api.register("blacklist_owner")
	target := api.register("blacklist_target")
	applicant := api.register("blacklist_applicant")

	room := api.createRoom(owner.Token, map[string]any{
		"name":        "BlacklistNeedle",
		"visibility":  "public",
		"join_policy": "approval_required",
	})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodGet, "/rooms/"+roomID+"/blacklist", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if items := response["blacklist"].([]any); len(items) != 0 {
		t.Fatalf("new room blacklist should start empty: %v", response)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/blacklist", owner.Token, map[string]any{
		"user_id": owner.User["id"].(string),
	})
	api.requireStatus(status, http.StatusConflict, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/blacklist", owner.Token, map[string]any{
		"user_id": super.User["id"].(string),
	})
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", applicant.Token, map[string]any{
		"reason": "please",
	})
	api.requireStatus(status, http.StatusAccepted, response)
	joinRequestID := response["join_request"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/join-requests?status=pending", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if requests := response["requests"].([]any); len(requests) != 1 {
		t.Fatalf("pending join request should be visible before block: %v", response)
	}
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/blacklist", owner.Token, map[string]any{
		"user_id": applicant.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/join-requests?status=pending", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if requests := response["requests"].([]any); len(requests) != 0 {
		t.Fatalf("blocked applicant should disappear from join requests: %v", response)
	}
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/join-requests/"+joinRequestID, owner.Token, map[string]any{
		"decision": "approve",
	})
	api.requireStatus(status, http.StatusConflict, response)
	status, response = api.request(http.MethodDelete, "/rooms/"+roomID+"/blacklist/"+applicant.User["id"].(string), owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/join-requests?status=pending", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if requests := response["requests"].([]any); len(requests) != 1 {
		t.Fatalf("unblocked applicant's pending request should reappear: %v", response)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": target.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	inviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodGet, "/room-invites?status=pending", target.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if invites := response["invites"].([]any); len(invites) != 1 || invites[0].(map[string]any)["id"] != inviteID {
		t.Fatalf("pending invite should be visible before block: %v", response)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/blacklist", owner.Token, map[string]any{
		"user_id": target.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	entry := response["entry"].(map[string]any)
	if entry["user"].(map[string]any)["id"] != target.User["id"] {
		t.Fatalf("block response should include blocked user entry: %v", response)
	}
	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/blacklist", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if items := response["blacklist"].([]any); len(items) != 1 || items[0].(map[string]any)["user"].(map[string]any)["id"] != target.User["id"] {
		t.Fatalf("blacklist should include target: %v", response)
	}
	status, response = api.request(http.MethodGet, "/room-invites?status=all", target.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if invites := response["invites"].([]any); len(invites) != 0 {
		t.Fatalf("blocked target should not see pending invite notifications: %v", response)
	}
	status, response = api.request(http.MethodPatch, "/room-invites/"+inviteID, target.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusConflict, response)
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": target.User["id"].(string),
	})
	api.requireStatus(status, http.StatusForbidden, response)
	status, response = api.request(http.MethodGet, "/rooms/search?q=BlacklistNeedle&limit=20", target.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if rooms := response["rooms"].([]any); len(rooms) != 0 {
		t.Fatalf("blocked target should not find the room in room search: %v", response)
	}
	status, response = api.request(http.MethodGet, "/search?q=BlacklistNeedle&categories=public_rooms&limit=20", target.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if rooms := response["public_rooms"].([]any); len(rooms) != 0 {
		t.Fatalf("blocked target should not find the room in global public search: %v", response)
	}
	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/join", target.Token, nil)
	api.requireStatus(status, http.StatusNotFound, response)

	otherRoom := api.createRoom(owner.Token, map[string]any{
		"name":        "OtherBlacklistNeedle",
		"visibility":  "public",
		"join_policy": "open",
	})
	status, response = api.request(http.MethodGet, "/rooms/search?q=OtherBlacklistNeedle&limit=20", target.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if rooms := response["rooms"].([]any); len(rooms) != 1 || rooms[0].(map[string]any)["id"] != otherRoom["id"] {
		t.Fatalf("blacklist should be room scoped: %v", response)
	}

	status, response = api.request(http.MethodDelete, "/rooms/"+roomID+"/blacklist/"+target.User["id"].(string), owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/room-invites?status=pending", target.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if invites := response["invites"].([]any); len(invites) != 1 || invites[0].(map[string]any)["id"] != inviteID {
		t.Fatalf("unblocked target should see the original pending invite again: %v", response)
	}
	status, response = api.request(http.MethodPatch, "/room-invites/"+inviteID, target.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusOK, response)
}

func TestRoomInvitesHideWhileJoinPolicyClosed(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("invite_policy_owner")
	target := api.register("invite_policy_target")

	room := api.createRoom(owner.Token, map[string]any{
		"name":        "Join Policy Invite",
		"join_policy": "approval_required",
	})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": target.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	inviteID := response["invite"].(map[string]any)["id"].(string)

	status, response = api.request(http.MethodGet, "/room-invites?status=pending", target.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if invites := response["invites"].([]any); len(invites) != 1 || invites[0].(map[string]any)["id"] != inviteID {
		t.Fatalf("pending invite should be visible before closing the room: %v", response)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/settings", owner.Token, map[string]any{
		"join_policy": "closed",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/room-invites?status=pending", target.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if invites := response["invites"].([]any); len(invites) != 0 {
		t.Fatalf("closed room should hide pending invite notifications: %v", response)
	}
	status, response = api.request(http.MethodGet, "/room-invites?status=all", target.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if invites := response["invites"].([]any); len(invites) != 0 {
		t.Fatalf("closed room should hide pending invites from all notifications too: %v", response)
	}

	status, response = api.request(http.MethodPatch, "/room-invites/"+inviteID, target.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusConflict, response)

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/settings", owner.Token, map[string]any{
		"join_policy": "approval_required",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/room-invites?status=pending", target.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if invites := response["invites"].([]any); len(invites) != 1 || invites[0].(map[string]any)["id"] != inviteID {
		t.Fatalf("reopening room should show the original pending invite again: %v", response)
	}

	status, response = api.request(http.MethodPatch, "/room-invites/"+inviteID, target.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusOK, response)
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
	if deletedInviterSummary["display_name"] != "用户已注销" || deletedInviterSummary["avatar_url"] != nil || deletedInviterSummary["is_deleted"] != true {
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
	deletedRoomSummary := deletedInvite["room"].(map[string]any)
	if deletedRoomSummary["name"] != "房间已删除" || deletedRoomSummary["rid"] != "" || deletedRoomSummary["is_deleted"] != true {
		t.Fatalf("deleted room invite should expose only a tombstone: %v", deletedInvite)
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
