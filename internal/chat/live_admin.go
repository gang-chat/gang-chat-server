package chat

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
)

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
	var res sql.Result
	switch req.Action {
	case "kick":
		res, _ = h.DB.Exec(`DELETE FROM live_participants WHERE room_id = ? AND user_id = ?`, roomID, targetID)
	case "mute_mic":
		res, _ = h.DB.Exec(`UPDATE live_participants SET mic_muted = 1, updated_at = ? WHERE room_id = ? AND user_id = ?`, nowMillis(), roomID, targetID)
	case "block_voice":
		res, _ = h.DB.Exec(
			`UPDATE live_participants
			 SET mic_muted = 1, headphones_muted = 1, voice_blocked = 1, updated_at = ?
			 WHERE room_id = ? AND user_id = ?`,
			nowMillis(), roomID, targetID,
		)
	case "restore_voice":
		res, _ = h.DB.Exec(`UPDATE live_participants SET voice_blocked = 0, updated_at = ? WHERE room_id = ? AND user_id = ?`, nowMillis(), roomID, targetID)
	}
	if res == nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "live moderation failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.jsonError(c, http.StatusNotFound, "not_found", "live participant not found")
		return
	}

	h.PublishLiveSnapshot(roomID, "live_participant_moderated", map[string]any{
		"user_id": targetID,
		"action":  req.Action,
	})

	c.JSON(http.StatusOK, gin.H{"ok": true, "action": req.Action, "user_id": targetID})
}
