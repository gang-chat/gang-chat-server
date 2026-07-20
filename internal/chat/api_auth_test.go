package chat

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"
)

func TestDeletedAccountRetainsMessageSenderSnapshotsAndAssets(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("history_snapshot_owner")
	sender := api.register("history_snapshot_sender")
	senderID := sender.User["id"].(string)
	room := api.createRoom(owner.Token, map[string]any{"name": "History Snapshot Room"})
	roomID := room["id"].(string)

	status, response := api.request(http.MethodPost, "/rooms/"+roomID+"/invites", owner.Token, map[string]any{
		"user_id": senderID,
	})
	api.requireStatus(status, http.StatusCreated, response)
	inviteID := response["invite"].(map[string]any)["id"].(string)
	status, response = api.request(http.MethodPatch, "/room-invites/"+inviteID, sender.Token, map[string]any{
		"decision": "accept",
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPatch, "/rooms/"+roomID+"/me", sender.Token, map[string]any{
		"room_display_name": "发送时房间名",
	})
	api.requireStatus(status, http.StatusOK, response)

	const assetID = "asset_history_sender_avatar"
	const avatarURL = "/assets/asset_history_sender_avatar/avatar.png"
	if _, err := api.db.Exec(
		`INSERT INTO assets (id, owner_user_id, purpose, filename, mime_type, size_bytes, url, created_at)
		 VALUES (?, ?, 'avatar', 'avatar.png', 'image/png', 3, ?, ?)`,
		assetID, senderID, avatarURL, nowMillis(),
	); err != nil {
		t.Fatalf("insert sender avatar asset: %v", err)
	}
	if _, err := api.db.Exec(
		`UPDATE users
		 SET display_name = '发送时用户名', avatar_url = ?, default_avatar_key = 'green-2'
		 WHERE id = ?`,
		avatarURL, senderID,
	); err != nil {
		t.Fatalf("set sender snapshot profile: %v", err)
	}

	status, response = api.request(http.MethodPost, "/rooms/"+roomID+"/messages", sender.Token, map[string]any{
		"client_message_id": "history_snapshot_message",
		"type":              "text",
		"body":              "retained-history-body",
		"mentions":          []any{},
		"attachments":       []any{},
	})
	api.requireStatus(status, http.StatusCreated, response)
	messageID := response["message"].(map[string]any)["id"].(string)

	if _, err := api.db.Exec(
		`UPDATE users
		 SET username = 'history_snapshot_renamed', username_normalized = 'history_snapshot_renamed',
		     display_name = '修改后的用户名', avatar_url = NULL, default_avatar_key = 'blue-1'
		 WHERE id = ?`,
		senderID,
	); err != nil {
		t.Fatalf("rename sender after message: %v", err)
	}
	if _, err := api.db.Exec(
		`UPDATE room_memberships SET room_display_name = '修改后的房间名'
		 WHERE room_id = ? AND user_id = ?`,
		roomID, senderID,
	); err != nil {
		t.Fatalf("rename sender in room after message: %v", err)
	}

	findMessage := func(response map[string]any) map[string]any {
		t.Helper()
		for _, raw := range response["messages"].([]any) {
			message := raw.(map[string]any)
			if message["id"] == messageID {
				return message
			}
		}
		t.Fatalf("message %s not found: %v", messageID, response)
		return nil
	}
	assertSnapshot := func(message map[string]any, deleted bool) {
		t.Helper()
		snapshot := message["sender"].(map[string]any)
		if snapshot["username"] != "history_snapshot_sender" ||
			snapshot["display_name"] != "发送时用户名" ||
			snapshot["room_display_name"] != "发送时房间名" ||
			snapshot["avatar_url"] != avatarURL ||
			snapshot["default_avatar_key"] != "green-2" {
			t.Fatalf("message should retain send-time sender snapshot: %v", snapshot)
		}
		if got, _ := snapshot["is_deleted"].(bool); got != deleted {
			t.Fatalf("unexpected sender deletion state: got %v want %v snapshot=%v", got, deleted, snapshot)
		}
	}

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/messages", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	assertSnapshot(findMessage(response), false)

	status, response = api.request(http.MethodDelete, "/users/me/account", sender.Token, map[string]any{"confirm": true})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/rooms/"+roomID+"/messages", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	assertSnapshot(findMessage(response), true)

	var messageCount int
	if err := api.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = ?`, messageID).Scan(&messageCount); err != nil || messageCount != 1 {
		t.Fatalf("historical message should remain after account deletion: count=%d err=%v", messageCount, err)
	}
	var assetOwner sql.NullString
	if err := api.db.QueryRow(`SELECT owner_user_id FROM assets WHERE id = ?`, assetID).Scan(&assetOwner); err != nil || assetOwner.Valid {
		t.Fatalf("historical avatar asset should remain detached: owner=%v err=%v", assetOwner, err)
	}

	status, response = api.request(http.MethodGet, "/users/"+senderID+"/profile", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	tombstone := response["profile"].(map[string]any)["user"].(map[string]any)
	if tombstone["display_name"] != "用户已注销" || tombstone["username"] != "已注销" || tombstone["is_deleted"] != true {
		t.Fatalf("deleted user profile should expose only a tombstone: %v", tombstone)
	}
	if _, ok := tombstone["common_rooms"]; ok {
		t.Fatalf("deleted user profile must not expose common rooms: %v", tombstone)
	}
	status, response = api.request(
		http.MethodGet,
		"/rooms/"+roomID+"/members/"+senderID+"/profile",
		owner.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, response)
	roomTombstone := response["profile"].(map[string]any)
	if roomTombstone["role"] != "deleted" || roomTombstone["room_display_name"] != nil {
		t.Fatalf("deleted historical sender should have no live room identity: %v", roomTombstone)
	}
	if user := roomTombstone["user"].(map[string]any); user["display_name"] != "用户已注销" || user["is_deleted"] != true {
		t.Fatalf("deleted historical sender should remain a room tombstone: %v", user)
	}

	status, response = api.request(http.MethodGet, "/users/search?q=history_snapshot_sender", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if users := response["users"].([]any); len(users) != 0 {
		t.Fatalf("deleted user must not be searchable: %v", response)
	}
	status, response = api.request(http.MethodGet, "/search?q=history_snapshot_sender&categories=messages", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if messages := response["messages"].([]any); len(messages) != 0 {
		t.Fatalf("sender snapshots must not be indexed as live users: %v", response)
	}
	status, response = api.request(http.MethodGet, "/search?q=retained-history-body&categories=messages", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if messages := response["messages"].([]any); len(messages) != 1 {
		t.Fatalf("retained message body should remain searchable: %v", response)
	}
}

func TestUsernameAvailability(t *testing.T) {
	api := newAPIHarness(t)
	api.register("availability_taken")

	status, response := api.request(
		http.MethodGet,
		"/auth/username-availability?username=available_name",
		"",
		nil,
	)
	if status != http.StatusOK || response["available"] != true {
		t.Fatalf("unused username should be available: status=%d response=%v", status, response)
	}

	status, response = api.request(
		http.MethodGet,
		"/auth/username-availability?username=AVAILABILITY_TAKEN",
		"",
		nil,
	)
	if status != http.StatusOK || response["available"] != false {
		t.Fatalf("taken username should be unavailable case-insensitively: status=%d response=%v", status, response)
	}

	status, response = api.request(
		http.MethodGet,
		"/auth/username-availability?username=bad.name",
		"",
		nil,
	)
	if status != http.StatusBadRequest || response["error"] == nil {
		t.Fatalf("invalid username should be rejected: status=%d response=%v", status, response)
	}
}

func TestEmailAvailability(t *testing.T) {
	api := newAPIHarness(t)
	api.register("email_availability_taken")

	status, response := api.request(
		http.MethodGet,
		"/auth/email-availability?email=available%40example.com",
		"",
		nil,
	)
	if status != http.StatusOK || response["available"] != true {
		t.Fatalf("unused email should be available: status=%d response=%v", status, response)
	}

	status, response = api.request(
		http.MethodGet,
		"/auth/email-availability?email=EMAIL_AVAILABILITY_TAKEN%40EXAMPLE.COM",
		"",
		nil,
	)
	if status != http.StatusOK || response["available"] != false {
		t.Fatalf("taken email should be unavailable case-insensitively: status=%d response=%v", status, response)
	}

	status, response = api.request(
		http.MethodGet,
		"/auth/email-availability?email=invalid",
		"",
		nil,
	)
	if status != http.StatusBadRequest || response["error"] == nil {
		t.Fatalf("invalid email should be rejected: status=%d response=%v", status, response)
	}
}

func TestPasswordResetFlow(t *testing.T) {
	api := newAPIHarness(t)
	api.register("password_reset_user")

	status, response := api.request(http.MethodPost, "/auth/password-reset/inspect", "", map[string]any{
		"login": "missing_password_reset_user",
	})
	if status != http.StatusNotFound || response["error"].(map[string]any)["code"] != "account_not_found" {
		t.Fatalf("missing account should be explicit: status=%d response=%v", status, response)
	}

	status, response = api.request(http.MethodPost, "/auth/password-reset/inspect", "", map[string]any{
		"login": "password_reset_user",
	})
	api.requireStatus(status, http.StatusOK, response)
	if response["can_send"] != true || response["retry_after"].(float64) != 0 {
		t.Fatalf("first inspection should allow sending: %v", response)
	}

	status, response = api.request(http.MethodPost, "/auth/password-reset/start", "", map[string]any{
		"login": "password_reset_user",
	})
	api.requireStatus(status, http.StatusOK, response)
	challengeID, _ := response["challenge_id"].(string)
	if challengeID == "" || response["masked_email"] != "p***r@example.com" {
		t.Fatalf("unexpected challenge response: %v", response)
	}
	if len(api.verificationEmail.sent) != 1 {
		t.Fatalf("want one email, got %d", len(api.verificationEmail.sent))
	}
	status, inspected := api.request(http.MethodPost, "/auth/password-reset/inspect", "", map[string]any{
		"login": "password_reset_user@example.com",
	})
	api.requireStatus(status, http.StatusOK, inspected)
	if inspected["can_send"] != false || inspected["challenge_id"] != challengeID || inspected["retry_after"].(float64) <= 0 {
		t.Fatalf("inspection should inherit the user's cooldown: %v", inspected)
	}

	status, repeated := api.request(http.MethodPost, "/auth/password-reset/start", "", map[string]any{
		"login": "password_reset_user@example.com",
	})
	api.requireStatus(status, http.StatusOK, repeated)
	if repeated["challenge_id"] != challengeID || len(api.verificationEmail.sent) != 1 {
		t.Fatalf("cooldown should reuse the sent challenge: response=%v emails=%d", repeated, len(api.verificationEmail.sent))
	}

	status, response = api.request(http.MethodPost, "/auth/password-reset/verify", "", map[string]any{
		"challenge_id": challengeID,
		"code":         api.verificationEmail.sent[0].code,
	})
	api.requireStatus(status, http.StatusOK, response)
	resetToken, _ := response["reset_token"].(string)
	if resetToken == "" {
		t.Fatalf("verification response missing reset token: %v", response)
	}

	status, response = api.request(http.MethodPost, "/auth/password-reset/complete", "", map[string]any{
		"reset_token":  resetToken,
		"new_password": "new correct horse battery staple",
	})
	api.requireStatus(status, http.StatusOK, response)

	status, _ = api.request(http.MethodPost, "/auth/login", "", map[string]any{
		"login": "password_reset_user", "password": "correct horse battery staple",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("old password should no longer work, got %d", status)
	}
	api.login("password_reset_user", "new correct horse battery staple")
}

func TestRegistrationEmailVerificationFlow(t *testing.T) {
	api := newAPIHarness(t)
	email := "registration_verification@example.com"

	status, response := api.request(http.MethodPost, "/auth/register", "", map[string]any{
		"username": "registration_verification",
		"email":    email,
		"password": "correct horse battery staple",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("registration without verification should fail: status=%d response=%v", status, response)
	}

	status, response = api.request(http.MethodPost, "/auth/email-verification/inspect", "", map[string]any{"email": email})
	api.requireStatus(status, http.StatusOK, response)
	if response["can_send"] != true {
		t.Fatalf("first inspection should permit sending: %v", response)
	}
	status, response = api.request(http.MethodPost, "/auth/email-verification/start", "", map[string]any{"email": email})
	api.requireStatus(status, http.StatusOK, response)
	challengeID := response["challenge_id"].(string)
	if len(api.verificationEmail.registrationSent) != 1 {
		t.Fatalf("want one registration email, got %d", len(api.verificationEmail.registrationSent))
	}
	status, inspected := api.request(http.MethodPost, "/auth/email-verification/inspect", "", map[string]any{"email": strings.ToUpper(email)})
	api.requireStatus(status, http.StatusOK, inspected)
	if inspected["can_send"] != false || inspected["challenge_id"] != challengeID || inspected["retry_after"].(float64) <= 0 {
		t.Fatalf("inspection should inherit the email cooldown: %v", inspected)
	}
	status, repeated := api.request(http.MethodPost, "/auth/email-verification/start", "", map[string]any{"email": email})
	api.requireStatus(status, http.StatusOK, repeated)
	if repeated["challenge_id"] != challengeID || len(api.verificationEmail.registrationSent) != 1 {
		t.Fatalf("cooldown should not send twice: response=%v emails=%d", repeated, len(api.verificationEmail.registrationSent))
	}

	status, response = api.request(http.MethodPost, "/auth/email-verification/verify", "", map[string]any{
		"challenge_id": challengeID,
		"code":         api.verificationEmail.registrationSent[0].code,
	})
	api.requireStatus(status, http.StatusOK, response)
	token := response["verification_token"].(string)
	status, response = api.request(http.MethodPost, "/auth/register", "", map[string]any{
		"username":                 "registration_verification",
		"email":                    email,
		"password":                 "correct horse battery staple",
		"email_verification_token": token,
	})
	api.requireStatus(status, http.StatusCreated, response)
}

func TestPasswordResetSessionGrantEndsWhenEmailChanges(t *testing.T) {
	api := newAPIHarness(t)
	user := api.register("password_reset_grant_user")

	status, response := api.request(http.MethodPost, "/auth/password-reset/start", "", map[string]any{
		"login": "password_reset_grant_user",
	})
	api.requireStatus(status, http.StatusOK, response)
	challengeID := response["challenge_id"].(string)
	code := api.verificationEmail.sent[0].code
	status, response = api.request(http.MethodPost, "/auth/password-reset/verify", "", map[string]any{
		"challenge_id": challengeID, "code": code,
	})
	api.requireStatus(status, http.StatusOK, response)
	resetToken := response["reset_token"].(string)
	status, response = api.request(http.MethodPost, "/auth/password-reset/claim", user.Token, map[string]any{
		"reset_token": resetToken,
	})
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodPost, "/auth/password", user.Token, map[string]any{
		"current_password": "", "new_password": "verified password one",
	})
	api.requireStatus(status, http.StatusOK, response)

	changedEmail := "password_reset_grant_changed@example.com"
	verificationToken := api.verifyEmail(changedEmail)
	status, response = api.request(http.MethodPatch, "/users/me/account", user.Token, map[string]any{
		"email":                    changedEmail,
		"email_verification_token": verificationToken,
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/auth/password", user.Token, map[string]any{
		"current_password": "", "new_password": "verified password two",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("email change should invalidate grant: status=%d response=%v", status, response)
	}
}

func TestAccountEmailChangeRequiresVerification(t *testing.T) {
	api := newAPIHarness(t)
	user := api.register("account_email_verification_user")
	changedEmail := "account_email_verification_changed@example.com"

	status, response := api.request(http.MethodPatch, "/users/me/account", user.Token, map[string]any{
		"email": changedEmail,
	})
	errorBody, _ := response["error"].(map[string]any)
	if status != http.StatusBadRequest || errorBody["code"] != "email_verification_required" {
		t.Fatalf("unverified email change should fail: status=%d response=%v", status, response)
	}

	verificationToken := api.verifyEmail(changedEmail)
	status, response = api.request(http.MethodPatch, "/users/me/account", user.Token, map[string]any{
		"email":                    changedEmail,
		"email_verification_token": verificationToken,
	})
	api.requireStatus(status, http.StatusOK, response)
	updated := response["user"].(map[string]any)
	if updated["email"] != changedEmail || updated["email_verified"] != true {
		t.Fatalf("verified email should be persisted as verified: %v", updated)
	}
}

func TestCurrentAccountCanVerifyItsExistingEmail(t *testing.T) {
	api := newAPIHarness(t)
	user := api.register("existing_email_verification_user")
	userID := user.User["id"].(string)
	email := user.User["email"].(string)
	if _, err := api.db.Exec(`UPDATE users SET email_verified = 0 WHERE id = ?`, userID); err != nil {
		t.Fatalf("mark existing email unverified: %v", err)
	}
	// Registration consumed its challenge. Expire only the resend cooldown so
	// this test isolates the authenticated existing-email permission contract.
	if _, err := api.db.Exec(
		`UPDATE email_verification_challenges SET resend_available_at = 0 WHERE email_normalized = ?`,
		strings.ToLower(email),
	); err != nil {
		t.Fatalf("expire registration verification cooldown: %v", err)
	}

	status, response := api.request(http.MethodPost, "/auth/email-verification/inspect", "", map[string]any{
		"email": email,
	})
	if status != http.StatusConflict {
		t.Fatalf("public registration verification should still reject a bound email: status=%d response=%v", status, response)
	}

	status, response = api.request(http.MethodPost, "/users/me/email-verification/inspect", user.Token, map[string]any{
		"email": email,
	})
	api.requireStatus(status, http.StatusOK, response)
	status, response = api.request(http.MethodPost, "/users/me/email-verification/start", user.Token, map[string]any{
		"email": email,
	})
	api.requireStatus(status, http.StatusOK, response)
	challengeID := response["challenge_id"].(string)
	code := api.verificationEmail.registrationSent[len(api.verificationEmail.registrationSent)-1].code
	status, response = api.request(http.MethodPost, "/auth/email-verification/verify", "", map[string]any{
		"challenge_id": challengeID,
		"code":         code,
	})
	api.requireStatus(status, http.StatusOK, response)
	verificationToken := response["verification_token"].(string)
	status, response = api.request(http.MethodPatch, "/users/me/account", user.Token, map[string]any{
		"email":                    email,
		"email_verification_token": verificationToken,
	})
	api.requireStatus(status, http.StatusOK, response)
	updated := response["user"].(map[string]any)
	if updated["email_verified"] != true {
		t.Fatalf("existing email should become verified: %v", updated)
	}
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
