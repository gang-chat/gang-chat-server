package chat

import (
	"net/http"
	"testing"
)

func TestUserSearchPagination(t *testing.T) {
	api := newAPIHarness(t)
	viewer := api.register("paged_user_viewer")
	api.register("paged_user_alpha")
	api.register("paged_user_beta")
	api.register("paged_user_gamma")

	status, first := api.request(
		http.MethodGet,
		"/users/search?q=paged_user_&limit=2",
		viewer.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, first)
	if got := len(first["users"].([]any)); got != 2 {
		t.Fatalf("first user search page length got %d want 2: %v", got, first)
	}
	if first["total_count"] != float64(4) {
		t.Fatalf("user search total_count got %v want 4: %v", first["total_count"], first)
	}
	cursor, ok := first["next_cursor"].(string)
	if !ok || cursor == "" {
		t.Fatalf("first user search page missing cursor: %v", first)
	}

	status, second := api.request(
		http.MethodGet,
		"/users/search?q=paged_user_&limit=2&cursor="+cursor,
		viewer.Token,
		nil,
	)
	api.requireStatus(status, http.StatusOK, second)
	if got := len(second["users"].([]any)); got != 2 {
		t.Fatalf("second user search page length got %d want 2: %v", got, second)
	}
	if second["next_cursor"] != nil {
		t.Fatalf("last user search page should not have cursor: %v", second)
	}

	status, response := api.request(
		http.MethodGet,
		"/users/search?q=paged_user_&cursor=invalid",
		viewer.Token,
		nil,
	)
	api.requireStatus(status, http.StatusBadRequest, response)
}

func TestSuperuserCanManageOtherUserCloudSettingsAndPassword(t *testing.T) {
	api := newAPIHarness(t)
	super := api.login("GANG", "64n9-Ch47")
	target := api.register("managed_settings_user")
	normal := api.register("managed_settings_outsider")
	targetID := target.User["id"].(string)

	status, response := api.request(
		http.MethodGet,
		"/users/"+targetID+"/settings",
		normal.Token,
		nil,
	)
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(
		http.MethodPatch,
		"/users/"+targetID+"/settings",
		super.Token,
		map[string]any{
			"display_name":        "Managed Display",
			"bio":                 "Managed Bio",
			"gender":              "female",
			"email":               "managed-settings-updated@example.test",
			"email_public":        true,
			"email_verified":      true,
			"phone_number":        "+86 13800000000",
			"phone_number_public": true,
			"language":            "en",
		},
	)
	api.requireStatus(status, http.StatusOK, response)
	user := response["user"].(map[string]any)
	if user["display_name"] != "Managed Display" ||
		user["bio"] != "Managed Bio" ||
		user["gender"] != "female" ||
		user["email"] != "managed-settings-updated@example.test" ||
		user["email_verified"] != true ||
		user["email_public"] != true ||
		user["phone_number_public"] != true ||
		user["language"] != "en" {
		t.Fatalf("forced user settings not returned: %v", user)
	}

	status, response = api.request(
		http.MethodPatch,
		"/users/"+targetID+"/audio-settings",
		super.Token,
		map[string]any{
			"default_audio_input_volume": 37,
			"live_music_output_volume":   12,
		},
	)
	api.requireStatus(status, http.StatusOK, response)
	audio := response["audio_settings"].(map[string]any)
	if audio["default_audio_input_volume"] != float64(37) ||
		audio["live_music_output_volume"] != float64(12) {
		t.Fatalf("forced audio settings not persisted: %v", audio)
	}

	status, response = api.request(
		http.MethodPost,
		"/users/"+targetID+"/password",
		normal.Token,
		map[string]any{"new_password": "new managed password"},
	)
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(
		http.MethodPost,
		"/users/"+targetID+"/password",
		super.Token,
		map[string]any{"new_password": "new managed password"},
	)
	api.requireStatus(status, http.StatusOK, response)

	status, response = api.request(http.MethodGet, "/me", target.Token, nil)
	api.requireStatus(status, http.StatusUnauthorized, response)
	api.login("managed_settings_user", "new managed password")
}

func TestSuperuserSettingsProtectSuperuserPasswordAndStatus(t *testing.T) {
	api := newAPIHarness(t)
	super := api.login("GANG", "64n9-Ch47")
	superID := super.User["id"].(string)

	status, response := api.request(
		http.MethodPost,
		"/users/"+superID+"/password",
		super.Token,
		map[string]any{"new_password": "cannot change here"},
	)
	api.requireStatus(status, http.StatusForbidden, response)

	status, response = api.request(
		http.MethodPatch,
		"/users/"+superID+"/settings",
		super.Token,
		map[string]any{"status": "suspended"},
	)
	api.requireStatus(status, http.StatusForbidden, response)
}

func TestSuperuserCanSuspendAndRestoreAccount(t *testing.T) {
	api := newAPIHarness(t)
	super := api.login("GANG", "64n9-Ch47")
	target := api.register("managed_suspension_user")
	targetID := target.User["id"].(string)
	stream := api.connectStream(targetID)

	status, response := api.request(
		http.MethodPatch,
		"/users/"+targetID+"/settings",
		super.Token,
		map[string]any{"status": "suspended"},
	)
	api.requireStatus(status, http.StatusOK, response)
	if user := response["user"].(map[string]any); user["status"] != "suspended" || user["display_name"] != "managed_suspension_user" {
		t.Fatalf("suspension should preserve the target profile: %v", user)
	}
	if payload := stream.await("account_suspended"); payload["reason"] != "账号已被封禁" {
		t.Fatalf("suspension should force every connected client to log out: %v", payload)
	}

	status, response = api.request(http.MethodGet, "/me", target.Token, nil)
	api.requireStatus(status, http.StatusUnauthorized, response)
	for attempt := 0; attempt < api.cfg.LoginMaxAttempts+1; attempt++ {
		status, response = api.request(http.MethodPost, "/auth/login", "", map[string]any{
			"login": "managed_suspension_user", "password": "correct horse battery staple",
		})
		api.requireStatus(status, http.StatusForbidden, response)
		if body := response["error"].(map[string]any); body["code"] != "account_suspended" || body["message"] != "账号已被封禁" {
			t.Fatalf("suspended login should return a specific error: %v", body)
		}
	}

	status, response = api.request(http.MethodGet, "/users/"+targetID+"/profile", super.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	profileUser := response["profile"].(map[string]any)["user"].(map[string]any)
	if profileUser["display_name"] != "managed_suspension_user" || profileUser["is_suspended"] != true || profileUser["is_deleted"] == true {
		t.Fatalf("suspended account should retain its normal profile with a suspension marker: %v", profileUser)
	}

	status, response = api.request(
		http.MethodPatch,
		"/users/"+targetID+"/settings",
		super.Token,
		map[string]any{"status": "active"},
	)
	api.requireStatus(status, http.StatusOK, response)
	if user := response["user"].(map[string]any); user["status"] != "active" {
		t.Fatalf("restored account should be active: %v", user)
	}

	// Restoring an account must not silently revive a session revoked by the
	// suspension; the user signs in again to establish a new session.
	status, response = api.request(http.MethodGet, "/me", target.Token, nil)
	api.requireStatus(status, http.StatusUnauthorized, response)
	api.login("managed_suspension_user", "correct horse battery staple")
}

func TestSuperuserUserSearchCanFindSuspendedButNotDeletedUsers(t *testing.T) {
	api := newAPIHarness(t)
	super := api.login("GANG", "64n9-Ch47")
	normal := api.register("status_search_viewer")
	suspended := api.register("status_search_suspended")
	deleted := api.register("status_search_deleted")
	if _, err := api.db.Exec(`UPDATE users SET status = 'suspended' WHERE id = ?`, suspended.User["id"]); err != nil {
		t.Fatalf("suspend search target: %v", err)
	}
	if _, err := api.db.Exec(`UPDATE users SET status = 'deleted', deleted_at = ? WHERE id = ?`, 1, deleted.User["id"]); err != nil {
		t.Fatalf("delete search target: %v", err)
	}

	status, response := api.request(http.MethodGet, "/users/search?q=status_search_suspended", normal.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["users"].([]any)); got != 0 {
		t.Fatalf("normal user should not find suspended account: %v", response)
	}
	status, response = api.request(http.MethodGet, "/users/search?q=status_search_suspended", super.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["users"].([]any)); got != 0 {
		t.Fatalf("ordinary superuser searches should still exclude suspended accounts: %v", response)
	}

	status, response = api.request(http.MethodGet, "/users/search?q=status_search_suspended&include_suspended=true", super.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["users"].([]any)); got != 1 {
		t.Fatalf("superuser should find suspended account: %v", response)
	}
	if user := response["users"].([]any)[0].(map[string]any); user["is_suspended"] != true {
		t.Fatalf("superuser search should identify the suspended account: %v", user)
	}

	status, response = api.request(http.MethodGet, "/users/search?q=status_search_deleted", super.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["users"].([]any)); got != 0 {
		t.Fatalf("deleted account must stay hidden from search: %v", response)
	}
}
