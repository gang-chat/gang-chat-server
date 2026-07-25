package livekitwebhook

import (
	"reflect"
	"strings"
	"testing"

	livekittoken "github.com/zhuangkaiyi/gang-chat/server/internal/livekit"
)

func TestParticipantLeftDeleteTargetsCurrentClientSession(t *testing.T) {
	query, args := participantLeftDelete(
		"room-1",
		"user-1",
		livekittoken.ClientLiveSessionMetadata("clive-old"),
		123,
	)
	if query == "" {
		t.Fatal("expected a delete query")
	}
	if !strings.Contains(query, "client_live_session_id = ?") {
		t.Fatalf("session-scoped delete missing client session guard: %q", query)
	}
	want := []any{"room-1", "user-1", "clive-old"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestParticipantLeftDeleteUsesJoinTimeForLegacyToken(t *testing.T) {
	query, args := participantLeftDelete("room-1", "user-1", "", 123)
	if !strings.Contains(query, "joined_at <= ?") {
		t.Fatalf("legacy delete missing join-time guard: %q", query)
	}
	want := []any{"room-1", "user-1", int64(123)}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("unexpected args: %#v", args)
	}
}
