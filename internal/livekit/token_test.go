package livekit

import (
	"testing"

	"github.com/livekit/protocol/auth"
)

func TestClientLiveSessionMetadataRoundTrip(t *testing.T) {
	metadata := ClientLiveSessionMetadata(" clive_reconnect ")
	if metadata != "gang-chat-client-live-session:clive_reconnect" {
		t.Fatalf("unexpected metadata: %q", metadata)
	}
	sessionID, ok := ClientLiveSessionIDFromMetadata(metadata)
	if !ok || sessionID != "clive_reconnect" {
		t.Fatalf("unexpected decoded session: %q, %v", sessionID, ok)
	}
	if _, ok := ClientLiveSessionIDFromMetadata("unrelated"); ok {
		t.Fatal("unrelated participant metadata must not be treated as a session")
	}
}

func TestGenerateJoinTokenIncludesMetadata(t *testing.T) {
	const secret = "01234567890123456789012345678901"
	token, err := GenerateJoinToken(TokenParams{
		APIKey:       "test-key",
		APISecret:    secret,
		Room:         "room-1",
		Identity:     "user-1",
		Name:         "User One",
		Metadata:     ClientLiveSessionMetadata("clive-1"),
		CanPublish:   true,
		CanSubscribe: true,
	})
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	verifier, err := auth.ParseAPIToken(token)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	_, grants, err := verifier.Verify(secret)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if grants.Metadata != ClientLiveSessionMetadata("clive-1") {
		t.Fatalf("unexpected token metadata: %q", grants.Metadata)
	}
}
