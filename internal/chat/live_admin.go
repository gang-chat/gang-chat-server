package chat

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// moderateLiveParticipant applies an admin moderation action to a live
// participant. Each action now drives the LiveKit media session through
// h.Live so it takes effect immediately, instead of only flipping a DB flag
// and waiting for the client to comply or the token to expire. The DB writes
// remain as the projection clients read over SSE, plus a room-scoped
// persistent policy that outlives the live session (see room_voice_bans).
//
// When h.Live is nil (dev mode without LiveKit credentials, or tests) the SDK
// calls are no-ops and we fall back to DB-only bookkeeping.
func (h *Handler) moderateLiveParticipant(c *gin.Context) {
	roomID := c.Param("room_id")
	actorID := currentUserID(c)
	if !h.isAdmin(roomID, actorID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	var req liveModerationRequest
	if err := c.ShouldBindJSON(&req); err != nil || !allowed(req.Action, "kick", "mute_mic", "block_voice", "restore_voice", "restore_headphones") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid live moderation action")
		return
	}
	targetID := c.Param("user_id")
	if h.isProtectedSuperuserTarget(actorID, targetID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot manage super user")
		return
	}

	// For everything except kick the target must currently be in live; kick
	// also accepts a participant that LiveKit still has but our row is stale.
	if req.Action != "kick" && !h.isLiveParticipant(roomID, targetID) {
		h.jsonError(c, http.StatusNotFound, "not_found", "live participant not found")
		return
	}

	switch req.Action {
	case "kick":
		if err := h.Live.RemoveParticipant(roomID, targetID); err != nil {
			h.jsonError(c, http.StatusBadGateway, "livekit_error", "failed to remove participant from live")
			return
		}
		res, _ := h.DB.Exec(`DELETE FROM live_participants WHERE room_id = ? AND user_id = ?`, roomID, targetID)
		if n, _ := res.RowsAffected(); n == 0 {
			h.jsonError(c, http.StatusNotFound, "not_found", "live participant not found")
			return
		}
		h.publishRoomUpdated(roomID)

	case "mute_mic":
		_, canSubscribe := h.liveKitMediaPermissions(roomID, targetID)
		if err := h.Live.SetMediaPermissions(roomID, targetID, false, canSubscribe); err != nil {
			h.jsonError(c, http.StatusBadGateway, "livekit_error", "failed to block microphone publishing")
			return
		}
		if err := h.Live.MuteMicrophone(roomID, targetID, true); err != nil {
			h.jsonError(c, http.StatusBadGateway, "livekit_error", "failed to mute microphone")
			return
		}
		now := nowMillis()
		_, _ = h.DB.Exec(
			`INSERT INTO room_voice_bans (
			   room_id, user_id, created_by_user_id, reason, created_at,
			   mic_blocked, headphones_blocked
			 )
			 VALUES (?, ?, ?, ?, ?, 1, 0)
			 ON CONFLICT(room_id, user_id) DO UPDATE SET
			   created_by_user_id = excluded.created_by_user_id,
			   reason = excluded.reason,
			   created_at = excluded.created_at,
			   mic_blocked = 1`,
			roomID, targetID, actorID, req.Reason, now,
		)
		_, _ = h.DB.Exec(
			`UPDATE live_participants
			 SET mic_muted = 1, mic_blocked = 1, voice_blocked = 1, updated_at = ?
			 WHERE room_id = ? AND user_id = ?`,
			now, roomID, targetID,
		)

	case "block_voice":
		// Revoke subscribe on the live session first so headphone mute takes
		// effect immediately without touching the target's microphone publish
		// permission. The room-scoped policy below makes it survive reconnects.
		canPublish, _ := h.liveKitMediaPermissions(roomID, targetID)
		if err := h.Live.SetMediaPermissions(roomID, targetID, canPublish, false); err != nil {
			h.jsonError(c, http.StatusBadGateway, "livekit_error", "failed to mute headphones")
			return
		}
		now := nowMillis()
		_, _ = h.DB.Exec(
			`INSERT INTO room_voice_bans (
			   room_id, user_id, created_by_user_id, reason, created_at,
			   mic_blocked, headphones_blocked
			 )
			 VALUES (?, ?, ?, ?, ?, 0, 1)
			 ON CONFLICT(room_id, user_id) DO UPDATE SET
			   created_by_user_id = excluded.created_by_user_id,
			   reason = excluded.reason,
			   created_at = excluded.created_at,
			   headphones_blocked = 1`,
			roomID, targetID, actorID, req.Reason, now,
		)
		_, _ = h.DB.Exec(
			`UPDATE live_participants
			 SET headphones_muted = 1, headphones_blocked = 1, updated_at = ?
			 WHERE room_id = ? AND user_id = ?`,
			now, roomID, targetID,
		)

	case "restore_voice":
		_, canSubscribe := h.liveKitMediaPermissions(roomID, targetID)
		if err := h.Live.SetMediaPermissions(roomID, targetID, true, canSubscribe); err != nil {
			h.jsonError(c, http.StatusBadGateway, "livekit_error", "failed to restore voice")
			return
		}
		if err := h.Live.MuteMicrophone(roomID, targetID, false); err != nil {
			h.jsonError(c, http.StatusBadGateway, "livekit_error", "failed to unmute microphone")
			return
		}
		_, _ = h.DB.Exec(
			`UPDATE room_voice_bans
			 SET mic_blocked = 0
			 WHERE room_id = ? AND user_id = ?`,
			roomID, targetID,
		)
		_, _ = h.DB.Exec(
			`DELETE FROM room_voice_bans
			 WHERE room_id = ? AND user_id = ?
			   AND mic_blocked = 0 AND headphones_blocked = 0`,
			roomID, targetID,
		)
		_, _ = h.DB.Exec(
			`UPDATE live_participants
			 SET mic_muted = 0, mic_blocked = 0,
			     voice_blocked = 0, updated_at = ?
			 WHERE room_id = ? AND user_id = ?`,
			nowMillis(), roomID, targetID,
		)

	case "restore_headphones":
		canPublish, _ := h.liveKitMediaPermissions(roomID, targetID)
		if err := h.Live.SetMediaPermissions(roomID, targetID, canPublish, true); err != nil {
			h.jsonError(c, http.StatusBadGateway, "livekit_error", "failed to restore headphones")
			return
		}
		_, _ = h.DB.Exec(
			`UPDATE room_voice_bans
			 SET headphones_blocked = 0
			 WHERE room_id = ? AND user_id = ?`,
			roomID, targetID,
		)
		_, _ = h.DB.Exec(
			`DELETE FROM room_voice_bans
			 WHERE room_id = ? AND user_id = ?
			   AND mic_blocked = 0 AND headphones_blocked = 0`,
			roomID, targetID,
		)
		_, _ = h.DB.Exec(
			`UPDATE live_participants
			 SET headphones_muted = 0, headphones_blocked = 0, updated_at = ?
			 WHERE room_id = ? AND user_id = ?`,
			nowMillis(), roomID, targetID,
		)
	}

	h.PublishLiveSnapshot(roomID, "live_participant_moderated", map[string]any{
		"user_id": targetID,
		"action":  req.Action,
	})

	c.JSON(http.StatusOK, gin.H{"ok": true, "action": req.Action, "user_id": targetID})
}

// isLiveParticipant reports whether a user currently has a live_participants
// row in the room. Used by moderation and member-volume handlers to validate a
// target is actually in the live session.
func (h *Handler) isLiveParticipant(roomID, userID string) bool {
	var count int
	_ = h.DB.QueryRow(
		`SELECT COUNT(*) FROM live_participants WHERE room_id = ? AND user_id = ? AND connection_state != 'left'`,
		roomID,
		userID,
	).Scan(&count)
	return count > 0
}
