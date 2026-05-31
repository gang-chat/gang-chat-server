package chat

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// moderateLiveParticipant applies an admin moderation action to a live
// participant. Each action now drives the LiveKit media session through
// h.Live so it takes effect immediately, instead of only flipping a DB flag
// and waiting for the client to comply or the token to expire. The DB writes
// remain as the projection clients read over SSE, plus — for voice bans — a
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
	if err := c.ShouldBindJSON(&req); err != nil || !allowed(req.Action, "kick", "mute_mic", "block_voice", "restore_voice") {
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

	case "mute_mic":
		if err := h.Live.MuteMicrophone(roomID, targetID, true); err != nil {
			h.jsonError(c, http.StatusBadGateway, "livekit_error", "failed to mute microphone")
			return
		}
		_, _ = h.DB.Exec(`UPDATE live_participants SET mic_muted = 1, updated_at = ? WHERE room_id = ? AND user_id = ?`, nowMillis(), roomID, targetID)

	case "block_voice":
		// Revoke publish on the live session first so it stops immediately,
		// then record the persistent ban so it survives reconnects, then
		// project onto the participant row clients render.
		if err := h.Live.SetCanPublish(roomID, targetID, false); err != nil {
			h.jsonError(c, http.StatusBadGateway, "livekit_error", "failed to block voice")
			return
		}
		_ = h.Live.MuteMicrophone(roomID, targetID, true)
		now := nowMillis()
		_, _ = h.DB.Exec(
			`INSERT INTO room_voice_bans (room_id, user_id, created_by_user_id, reason, created_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(room_id, user_id) DO UPDATE SET
			   created_by_user_id = excluded.created_by_user_id,
			   reason = excluded.reason,
			   created_at = excluded.created_at`,
			roomID, targetID, actorID, req.Reason, now,
		)
		_, _ = h.DB.Exec(
			`UPDATE live_participants
			 SET mic_muted = 1, headphones_muted = 1, voice_blocked = 1, updated_at = ?
			 WHERE room_id = ? AND user_id = ?`,
			now, roomID, targetID,
		)

	case "restore_voice":
		if err := h.Live.SetCanPublish(roomID, targetID, true); err != nil {
			h.jsonError(c, http.StatusBadGateway, "livekit_error", "failed to restore voice")
			return
		}
		_, _ = h.DB.Exec(`DELETE FROM room_voice_bans WHERE room_id = ? AND user_id = ?`, roomID, targetID)
		_, _ = h.DB.Exec(`UPDATE live_participants SET voice_blocked = 0, updated_at = ? WHERE room_id = ? AND user_id = ?`, nowMillis(), roomID, targetID)
	}

	h.PublishLiveSnapshot(roomID, "live_participant_moderated", map[string]any{
		"user_id": targetID,
		"action":  req.Action,
	})

	c.JSON(http.StatusOK, gin.H{"ok": true, "action": req.Action, "user_id": targetID})
}
