package livekit

import (
	"time"

	"github.com/livekit/protocol/auth"
)

type TokenParams struct {
	APIKey       string
	APISecret    string
	Room         string
	Identity     string
	Name         string
	CanPublish   bool
	CanSubscribe bool
	TTL          time.Duration
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

	return at.ToJWT()
}
