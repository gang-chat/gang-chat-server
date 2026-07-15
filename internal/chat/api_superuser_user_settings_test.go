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
			"email_public":        true,
			"email_verified":      false,
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
		user["email_verified"] != false ||
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

	status, response = api.request(http.MethodGet, "/users/search?q=status_search_deleted", super.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if got := len(response["users"].([]any)); got != 0 {
		t.Fatalf("deleted account must stay hidden from search: %v", response)
	}
}
