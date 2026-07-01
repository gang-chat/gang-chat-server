package livekit

import (
	"context"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// Controller wraps a LiveKit RoomServiceClient with the room-moderation
// primitives the chat layer needs. This is the server-authoritative path:
// every action here takes effect on the live media session immediately,
// instead of relying on the client to behave or on a short token TTL to
// eventually re-evaluate permissions.
//
// In our deployment the LiveKit room name == business room id and the
// participant identity == user_id (see livekit/token.go and live_core.go),
// so callers pass those straight through.
type Controller struct {
	rooms *lksdk.RoomServiceClient
}

// NewController returns a Controller, or nil if LiveKit is not configured.
// A nil Controller is a valid value: every method is a no-op on a nil
// receiver, so callers in dev mode (no API key) or tests degrade to
// DB-only behavior without nil checks at each call site.
func NewController(rooms *lksdk.RoomServiceClient) *Controller {
	if rooms == nil {
		return nil
	}
	return &Controller{rooms: rooms}
}

func (c *Controller) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// RemoveParticipant force-disconnects a participant from the room. This is
// the real "kick": the WebRTC session is torn down server-side rather than
// just deleting our DB row and waiting for the token to expire.
func (c *Controller) RemoveParticipant(room, identity string) error {
	if c == nil {
		return nil
	}
	ctx, cancel := c.ctx()
	defer cancel()
	_, err := c.rooms.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{
		Room:     room,
		Identity: identity,
	})
	return err
}

// SetCanPublish flips a participant's publish permission live. Setting it to
// false revokes the ability to push any track (mic, camera, screen share)
// without waiting for token renewal; LiveKit pushes a permissions-changed
// event to the client and stops accepting their media.
func (c *Controller) SetCanPublish(room, identity string, canPublish bool) error {
	if c == nil {
		return nil
	}
	ctx, cancel := c.ctx()
	defer cancel()
	info, err := c.rooms.GetParticipant(ctx, &livekit.RoomParticipantIdentity{
		Room:     room,
		Identity: identity,
	})
	if err != nil {
		return err
	}
	perm := info.GetPermission()
	if perm == nil {
		perm = &livekit.ParticipantPermission{CanSubscribe: true}
	}
	perm.CanPublish = canPublish
	_, err = c.rooms.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
		Room:       room,
		Identity:   identity,
		Permission: perm,
	})
	return err
}

// SetMediaPermissions writes the complete publish/subscribe permission pair in
// one LiveKit update. Moderation callers use this when changing either mic or
// headphones so the untouched permission cannot be reset by an omitted/default
// protobuf field.
func (c *Controller) SetMediaPermissions(room, identity string, canPublish, canSubscribe bool) error {
	if c == nil {
		return nil
	}
	ctx, cancel := c.ctx()
	defer cancel()
	info, err := c.rooms.GetParticipant(ctx, &livekit.RoomParticipantIdentity{
		Room:     room,
		Identity: identity,
	})
	if err != nil {
		return err
	}
	perm := info.GetPermission()
	if perm == nil {
		perm = &livekit.ParticipantPermission{}
	}
	perm.CanPublish = canPublish
	perm.CanSubscribe = canSubscribe
	_, err = c.rooms.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
		Room:       room,
		Identity:   identity,
		Permission: perm,
	})
	return err
}

// SetCanSubscribe flips a participant's subscribe permission live. This is the
// enforcement primitive for admin "headphone mute": the target stays in the
// room but cannot receive other participants' media until restored.
func (c *Controller) SetCanSubscribe(room, identity string, canSubscribe bool) error {
	if c == nil {
		return nil
	}
	ctx, cancel := c.ctx()
	defer cancel()
	info, err := c.rooms.GetParticipant(ctx, &livekit.RoomParticipantIdentity{
		Room:     room,
		Identity: identity,
	})
	if err != nil {
		return err
	}
	perm := info.GetPermission()
	if perm == nil {
		perm = &livekit.ParticipantPermission{CanPublish: true}
	}
	perm.CanSubscribe = canSubscribe
	_, err = c.rooms.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
		Room:       room,
		Identity:   identity,
		Permission: perm,
	})
	return err
}

// MuteMicrophone server-side mutes (or unmutes) a participant's microphone
// track. Unlike a client-driven mute this cannot be undone by the target;
// it's the enforcement primitive behind admin "mute mic".
//
// Returns nil (not an error) when the participant has no published mic track
// yet — there is simply nothing to mute, and the publish guard / ban table
// covers the case where they publish later.
func (c *Controller) MuteMicrophone(room, identity string, muted bool) error {
	if c == nil {
		return nil
	}
	ctx, cancel := c.ctx()
	defer cancel()
	info, err := c.rooms.GetParticipant(ctx, &livekit.RoomParticipantIdentity{
		Room:     room,
		Identity: identity,
	})
	if err != nil {
		return err
	}
	for _, track := range info.GetTracks() {
		if track.GetType() != livekit.TrackType_AUDIO {
			continue
		}
		if track.GetSource() != livekit.TrackSource_MICROPHONE && track.GetSource() != livekit.TrackSource_UNKNOWN {
			continue
		}
		_, err := c.rooms.MutePublishedTrack(ctx, &livekit.MuteRoomTrackRequest{
			Room:     room,
			Identity: identity,
			TrackSid: track.GetSid(),
			Muted:    muted,
		})
		if err != nil {
			return err
		}
	}
	return nil
}
