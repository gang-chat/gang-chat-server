package chat

import (
	"github.com/zhuangkaiyi/gang-chat/server/internal/idgen"
	"github.com/zhuangkaiyi/gang-chat/server/internal/releases"
	"net/http"
	"testing"
)

func TestAppVersionEndpoint(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("version_owner")

	status, response := api.request(http.MethodGet, "/app/version", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if response["latest_version"] != latestClientVersion {
		t.Fatalf("latest version mismatch: %v", response)
	}
	if response["minimum_supported_version"] != latestClientVersion {
		t.Fatalf("minimum version mismatch: %v", response)
	}
}

func TestAppVersionReportsNewestPublishedRelease(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("version_reader")
	for _, release := range []releases.Release{
		{Version: "0.9.10", Published: true, MinimumSupportedVersion: "0.9.0"},
		{Version: "0.10.1", Published: true},
		{Version: "0.11.0", Published: false},
	} {
		if err := releases.Save(api.db, release); err != nil {
			t.Fatal(err)
		}
	}

	status, response := api.request(http.MethodGet, "/app/version", owner.Token, nil)
	api.requireStatus(status, http.StatusOK, response)
	if response["latest_version"] != "0.10.1" || response["minimum_supported_version"] != "0.10.1" {
		t.Fatalf("expected the newest published release, got %v", response)
	}
}

func TestPublicUIDAndRIDRanges(t *testing.T) {
	api := newAPIHarness(t)
	owner := api.register("owner_ids")
	other := api.register("other_ids")

	ownerUID := parseNumericID(t, owner.User["uid"])
	otherUID := parseNumericID(t, other.User["uid"])
	if ownerUID < idgen.UserUIDStart ||
		ownerUID >= idgen.ReservedSuperUIDValue ||
		otherUID <= ownerUID ||
		otherUID >= idgen.ReservedSuperUIDValue {
		t.Fatalf("unexpected uid sequence: owner=%d other=%d", ownerUID, otherUID)
	}

	room := api.createRoom(owner.Token, map[string]any{"name": "ID Range", "join_policy": "open"})
	rid := parseNumericID(t, room["rid"])
	if rid < idgen.RoomRIDStart {
		t.Fatalf("rid below configured range: %d", rid)
	}
}
