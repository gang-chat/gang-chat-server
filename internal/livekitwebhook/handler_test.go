package livekitwebhook

import (
	"reflect"
	"strings"
	"testing"

	livekittoken "github.com/zhuangkaiyi/gang-chat/server/internal/livekit"
)

func TestParticipantLeftMarksCurrentClientSessionReconnecting(t *testing.T) {
	query, args := participantLeftReconnectUpdate(
		"room-1",
		"user-1",
		livekittoken.ClientLiveSessionMetadata("clive-old"),
		123,
		456,
	)
	if query == "" {
		t.Fatal("expected an update query")
	}
	if !strings.Contains(query, "connection_state = 'reconnecting'") ||
		!strings.Contains(query, "client_live_session_id = ?") {
		t.Fatalf("session-scoped delete missing client session guard: %q", query)
	}
	want := []any{int64(456), "room-1", "user-1", "clive-old"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestParticipantLeftReconnectUsesJoinTimeForLegacyToken(t *testing.T) {
	query, args := participantLeftReconnectUpdate("room-1", "user-1", "", 123, 456)
	if !strings.Contains(query, "joined_at <= ?") {
		t.Fatalf("legacy update missing join-time guard: %q", query)
	}
	want := []any{int64(456), "room-1", "user-1", int64(123)}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestParticipantRejoinedOnlyRestoresMatchingReconnect(t *testing.T) {
	query, args := participantRejoinedUpdate(
		"room-1",
		"user-1",
		livekittoken.ClientLiveSessionMetadata("clive-current"),
		123,
		789,
	)
	if !strings.Contains(query, "connection_state = 'online'") ||
		!strings.Contains(query, "connection_state = 'reconnecting'") ||
		!strings.Contains(query, "client_live_session_id = ?") {
		t.Fatalf("rejoin update is not reconnect-session scoped: %q", query)
	}
	want := []any{int64(789), "room-1", "user-1", "clive-current"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("unexpected args: %#v", args)
	}
}
