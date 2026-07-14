package chat

import (
	"database/sql"
	"net/http"
	"testing"
)

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
	if deletedReviewer["display_name"] != "用户已注销" || deletedReviewer["avatar_url"] != nil || deletedReviewer["is_deleted"] != true {
		t.Fatalf("deleted reviewer summary should be a placeholder: %v", deletedReviewer)
	}
}

func TestRoomEventNotifications(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("room_event_owner")
	member := api.register("room_event_member")
	nextOwner := api.register("room_event_next_owner")
	room := api.createRoom(owner.Token, map[string]any{"name": "Event Room", "description": "Room event bio"})
	roomID := room["id"].(string)

	inviteAndAccept := func(user testSession) {
		t.Helper()
		status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
			"user_id": user.User["id"].(string),
		})
		api.requireStatus(status, http.StatusCreated, response)
		inviteID := response["invite"].(map[string]any)["id"].(string)
		status, response = api.request(http.MethodPatch, "/room-invites/"+inviteID, user.Token, map[string]any{"decision": "accept"})
		api.requireStatus(status, http.StatusOK, response)
	}
	inviteAndAccept(member)
	inviteAndAccept(nextOwner)

	status, response := api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+member.User["id"].(string), owner.Token, map[string]any{
		"role": "admin",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/room-notifications", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	notifications := response["notifications"].([]any)
	if len(notifications) != 1 {
		t.Fatalf("promotion should create one room notification: %v", response)
	}
	promotion := notifications[0].(map[string]any)
	if promotion["type"] != roomNotificationRolePromoted || promotion["to_role"] != "admin" {
		t.Fatalf("promotion notification mismatch: %v", promotion)
	}
	if promotion["read_at"] != nil {
		t.Fatalf("new promotion notification should be unread: %v", promotion)
	}
	if promotion["actor"].(map[string]any)["room_role"] != "owner" {
		t.Fatalf("promotion actor should include room role: %v", promotion)
	}
	promotionMessageID, _ := promotion["message_id"].(string)
	if promotionMessageID == "" {
		t.Fatalf("promotion notification should include message_id: %v", promotion)
	}
	if _, err := api.db.Exec(`UPDATE room_notifications SET message_id = NULL WHERE id = ?`, promotion["id"].(string)); err != nil {
		t.Fatalf("clear promotion message_id: %v", err)
	}
	status, response = api.request(http.MethodGet, "/room-notifications", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	promotion = response["notifications"].([]any)[0].(map[string]any)
	if promotion["message_id"] != promotionMessageID {
		t.Fatalf("legacy promotion notification should resolve message_id: %v", promotion)
	}
	var storedPromotionMessageID sql.NullString
	if err := api.db.QueryRow(`SELECT message_id FROM room_notifications WHERE id = ?`, promotion["id"].(string)).Scan(&storedPromotionMessageID); err != nil {
		t.Fatalf("read backfilled promotion message_id: %v", err)
	}
	if !storedPromotionMessageID.Valid || storedPromotionMessageID.String != promotionMessageID {
		t.Fatalf("promotion message_id should be backfilled in storage: %v", storedPromotionMessageID)
	}
	if roomPayload := promotion["room"].(map[string]any); roomPayload["name"] != "Event Room" || roomPayload["description"] != "Room event bio" {
		t.Fatalf("promotion should include room payload: %v", promotion)
	}
	status, response = api.request(http.MethodPost, "/room-notifications/read", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/room-notifications", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	promotion = response["notifications"].([]any)[0].(map[string]any)
	if promotion["read_at"] == nil {
		t.Fatalf("mark read should stamp room notification: %v", promotion)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+member.User["id"].(string), owner.Token, map[string]any{
		"role": "member",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodDelete, "/rooms/"+roomID+"/members/"+member.User["id"].(string), owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/room-notifications", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	notifications = response["notifications"].([]any)
	if len(notifications) != 3 {
		t.Fatalf("member should retain role and removal notifications: %v", response)
	}
	notificationByType := func(items []any, notificationType string) map[string]any {
		t.Helper()
		for _, item := range items {
			notification := item.(map[string]any)
			if notification["type"] == notificationType {
				return notification
			}
		}
		t.Fatalf("missing notification type %s in %v", notificationType, items)
		return nil
	}
	removed := notificationByType(notifications, roomNotificationMemberRemoved)
	if removed["type"] != roomNotificationMemberRemoved || removed["room_exists"] != true {
		t.Fatalf("removal notification mismatch: %v", removed)
	}
	if removed["room"].(map[string]any)["joined"] != false {
		t.Fatalf("removed member should not be joined in notification room payload: %v", removed)
	}
	demotion := notificationByType(notifications, roomNotificationRoleDemoted)
	if demotion["type"] != roomNotificationRoleDemoted || demotion["from_role"] != "admin" || demotion["to_role"] != "member" {
		t.Fatalf("demotion notification mismatch: %v", demotion)
	}

	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/creator", owner.Token, map[string]any{
		"user_id": nextOwner.User["id"].(string),
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/room-notifications", nextOwner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	notifications = response["notifications"].([]any)
	if len(notifications) != 1 || notifications[0].(map[string]any)["type"] != roomNotificationRolePromoted {
		t.Fatalf("new creator should receive promotion notification: %v", response)
	}
	status, response = api.request(http.MethodGet, "/room-notifications", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	notifications = response["notifications"].([]any)
	if len(notifications) != 1 {
		t.Fatalf("previous creator should receive self demotion notification: %v", response)
	}
	selfDemotion := notifications[0].(map[string]any)
	if selfDemotion["type"] != roomNotificationCreatorTransferDemoted || selfDemotion["actor"] != nil || selfDemotion["to_role"] != "admin" {
		t.Fatalf("creator transfer demotion notification mismatch: %v", selfDemotion)
	}
}

func TestDeleteRoomNotificationsOnlyHidesCurrentUsersFeed(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("notification_delete_owner")
	member := api.register("notification_delete_member")
	room := api.createRoom(owner.Token, map[string]any{"name": "Notification Delete Room"})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": member.User["id"].(string),
	})
	api.requireStatus(status, http.StatusCreated, response)
	inviteID := response["invite"].(map[string]any)["id"].(string)

	status, response = api.request(
		http.MethodDelete,
		"/room-notifications/"+roomNotificationDeletionInvite+"/"+inviteID,
		member.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/room-invites?status=all", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if invites := response["invites"].([]any); len(invites) != 0 {
		t.Fatalf("deleted invite notification should be hidden: %v", response)
	}
	var inviteCount int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM room_invites WHERE id = ?`, inviteID).Scan(&inviteCount); err != nil {
		t.Fatalf("count invite source record: %v", err)
	}
	if inviteCount != 1 {
		t.Fatalf("deleting an invite notification must not delete the invite record, got %d", inviteCount)
	}

	status, response = api.request(http.MethodPatch, "/room-invites/"+inviteID, member.Token, map[string]any{"decision": "accept"})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/members/"+member.User["id"].(string), owner.Token, map[string]any{
		"role": "admin",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/room-notifications", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	notifications := response["notifications"].([]any)
	if len(notifications) != 1 {
		t.Fatalf("expected one role notification before deletion: %v", response)
	}
	notificationID := notifications[0].(map[string]any)["id"].(string)
	var sourceNotificationCount, messageCountBefore int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM room_notifications WHERE id = ?`, notificationID).Scan(&sourceNotificationCount); err != nil {
		t.Fatalf("count event notification source record: %v", err)
	}
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE room_id = ?`, roomID).Scan(&messageCountBefore); err != nil {
		t.Fatalf("count room system messages before deletion: %v", err)
	}

	status, response = api.request(
		http.MethodDelete,
		"/room-notifications/"+roomNotificationDeletionRoomEvent+"/"+notificationID,
		member.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodGet, "/room-notifications", member.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if notifications := response["notifications"].([]any); len(notifications) != 0 {
		t.Fatalf("deleted room event notification should be hidden: %v", response)
	}
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM room_notifications WHERE id = ?`, notificationID).Scan(&sourceNotificationCount); err != nil {
		t.Fatalf("recount event notification source record: %v", err)
	}
	if sourceNotificationCount != 1 {
		t.Fatalf("deleting a notification must not delete its source record, got %d", sourceNotificationCount)
	}
	var messageCountAfter int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE room_id = ?`, roomID).Scan(&messageCountAfter); err != nil {
		t.Fatalf("count room system messages after deletion: %v", err)
	}
	if messageCountAfter != messageCountBefore {
		t.Fatalf("deleting a notification must not change room messages: before=%d after=%d", messageCountBefore, messageCountAfter)
	}

	status, response = api.request(
		http.MethodDelete,
		"/room-notifications/"+roomNotificationDeletionRoomEvent+"/"+notificationID,
		owner.Token,
		nil,
	)
	if status != http.StatusNotFound {
		t.Fatalf("another user must not delete this notification, got status=%d response=%v", status, response)
	}
}

func TestDeleteRoomApplicationNotificationKeepsApplicationRecord(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("app_notice_delete_owner")
	applicant := api.register("app_notice_delete_applicant")
	room := api.createRoom(owner.Token, map[string]any{
		"name":        "Application Notification Delete Room",
		"join_policy": "approval_required",
	})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/join", applicant.Token, nil)
	api.requireStatus(status, http.StatusAccepted, response)
	requestID := response["join_request"].(map[string]any)["id"].(string)

	status, response = api.request(
		http.MethodDelete,
		"/room-notifications/"+roomNotificationDeletionApplicationRequested+"/"+requestID,
		applicant.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	var requestCount int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM join_requests WHERE id = ?`, requestID).Scan(&requestCount); err != nil {
		t.Fatalf("count room application source record: %v", err)
	}
	if requestCount != 1 {
		t.Fatalf("deleting an application notification must not delete the application record, got %d", requestCount)
	}

	status, response = api.request(http.MethodGet, "/room-applications?status=all", applicant.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	applications := response["applications"].([]any)
	if len(applications) != 1 {
		t.Fatalf("application source record should remain available: %v", response)
	}
	application := applications[0].(map[string]any)
	if application["request_notification_deleted"] != true || application["review_notification_deleted"] != false {
		t.Fatalf("application notification deletion flags mismatch: %v", application)
	}
}
