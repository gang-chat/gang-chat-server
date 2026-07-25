package livekit

import (
	"strings"
	"time"

	"github.com/livekit/protocol/auth"
)

const clientLiveSessionMetadataPrefix = "gang-chat-client-live-session:"

type TokenParams struct {
	APIKey       string
	APISecret    string
	Room         string
	Identity     string
	Name         string
	Metadata     string
	CanPublish   bool
	CanSubscribe bool
	TTL          time.Duration
}

func ClientLiveSessionMetadata(clientLiveSessionID string) string {
	clientLiveSessionID = strings.TrimSpace(clientLiveSessionID)
	if clientLiveSessionID == "" {
		return ""
	}
	return clientLiveSessionMetadataPrefix + clientLiveSessionID
}

func ClientLiveSessionIDFromMetadata(metadata string) (string, bool) {
	if !strings.HasPrefix(metadata, clientLiveSessionMetadataPrefix) {
		return "", false
	}
	clientLiveSessionID := strings.TrimSpace(
		strings.TrimPrefix(metadata, clientLiveSessionMetadataPrefix),
	)
	return clientLiveSessionID, clientLiveSessionID != ""
}

func GenerateJoinToken(params TokenParams) (string, error) {
	at := auth.NewAccessToken(params.APIKey, params.APISecret)

	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     params.Room,
	}
	grant.SetCanPublish(params.CanPublish)
	grant.SetCanSubscribe(params.CanSubscribe)

	ttl := params.TTL
	if ttl == 0 {
		ttl = 24 * time.Hour
	}

	at.SetVideoGrant(grant).
		SetIdentity(params.Identity).
		SetName(params.Name).
		SetValidFor(ttl)
	if params.Metadata != "" {
		at.SetMetadata(params.Metadata)
	}

	return at.ToJWT()
}
