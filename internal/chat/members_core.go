package chat

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
)

func (h *Handler) listMembers(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}

	limit := parseLimit(c.Query("limit"), 50, 100)
	rows, err := h.DB.Query(
		`SELECT u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
		        rm.role, rm.text_muted_until, rm.joined_at
		 FROM room_memberships rm
		 JOIN users u ON u.id = rm.user_id
		 WHERE rm.room_id = ?
		 ORDER BY rm.joined_at ASC
		 LIMIT ?`,
		roomID, limit,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to list members")
		return
	}
	defer rows.Close()

	members := make([]currentMember, 0)
	for rows.Next() {
		var id, uid, username string
		var displayName, avatarURL, defaultAvatar sql.NullString
		var role string
		var textMutedUntil sql.NullInt64
		var joinedAt int64
		if err := rows.Scan(&id, &uid, &username, &displayName, &avatarURL, &defaultAvatar, &role, &textMutedUntil, &joinedAt); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read members")
			return
		}
		var mutedUntil *string
		if textMutedUntil.Valid {
			v := formatMillis(textMutedUntil.Int64)
			mutedUntil = &v
		}
		members = append(members, currentMember{
			User:           summaryFromUserFields(id, uid, username, displayName, avatarURL, defaultAvatar),
			Role:           role,
			TextMutedUntil: mutedUntil,
			JoinedAt:       formatMillis(joinedAt),
		})
	}
	c.JSON(http.StatusOK, gin.H{"members": members, "next_cursor": nil})
}

func (h *Handler) joinRoom(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.roomExists(c, roomID) {
		return
	}
	if !h.bindOptionalJSON(c, nil) {
		return
	}
	userID := currentUserID(c)
	isSuperuser := h.isSuperuser(userID)
	var policy string
	if err := h.DB.QueryRow(`SELECT join_policy FROM rooms WHERE id = ?`, roomID).Scan(&policy); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room")
		return
	}
	var alreadyMember int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&alreadyMember)
	if alreadyMember == 0 && policy == "approval_required" && !isSuperuser {
		now := nowMillis()
		id := newID("jrq")
		_, err := h.DB.Exec(
			`INSERT INTO join_requests (id, room_id, user_id, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'pending', ?, ?)
			 ON CONFLICT(room_id, user_id) DO UPDATE SET updated_at = excluded.updated_at`,
			id, roomID, userID, now, now,
		)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to create join request")
			return
		}
		var requestID, status string
		var createdAt int64
		_ = h.DB.QueryRow(`SELECT id, status, created_at FROM join_requests WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&requestID, &status, &createdAt)
		c.JSON(http.StatusAccepted, gin.H{"join_request": gin.H{"id": requestID, "room_id": roomID, "status": status, "created_at": formatMillis(createdAt)}})
		return
	}
	if alreadyMember == 0 && policy == "closed" && !isSuperuser {
		h.jsonError(c, http.StatusForbidden, "forbidden", "room is not accepting joins")
		return
	}
	if alreadyMember != 0 && isSuperuser {
		_, _ = h.DB.Exec(`UPDATE room_memberships SET role = 'admin' WHERE room_id = ? AND user_id = ? AND role != 'owner'`, roomID, userID)
	}

	role := "member"
	if isSuperuser {
		role = "admin"
	}
	_, err := h.DB.Exec(
		`INSERT INTO room_memberships (room_id, user_id, role, joined_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(room_id, user_id) DO NOTHING`,
		roomID, userID, role, nowMillis(),
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to join room")
		return
	}

	detail, err := h.buildRoomDetail(roomID, userID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room")
		return
	}
	// New member gets the room added to their list; everyone already in the
	// room gets an updated snapshot (member_count went up). Exclude the joiner
	// from the update so they don't get both events for the same change.
	h.publishRoomToUser(userID, roomID, "room_added")
	h.publishRoomUpdated(roomID, userID)
	c.JSON(http.StatusOK, gin.H{"room": detail})
}

func (h *Handler) leaveRoom(c *gin.Context) {
	roomID := c.Param("room_id")
	userID := currentUserID(c)
	if !h.requireMember(c, roomID) {
		return
	}
	if !h.bindOptionalJSON(c, nil) {
		return
	}

	tx, err := h.DB.Begin()
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to leave room")
		return
	}
	defer tx.Rollback()

	_, _ = tx.Exec(`DELETE FROM live_participants WHERE room_id = ? AND user_id = ?`, roomID, userID)
	_, err = tx.Exec(`DELETE FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, userID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to leave room")
		return
	}
	pruned, err := h.pruneOrRepairRoomTx(tx, roomID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to repair room admins")
		return
	}
	if err := tx.Commit(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to save room state")
		return
	}
	// The leaver always drops the room from their list. If the room still
	// exists, surviving members get a fresh snapshot (member_count down, and
	// possibly a repaired admin). If it was pruned to empty there's no one left
	// to notify.
	h.publishRoomDeleted(roomID, userID)
	if !pruned {
		h.publishRoomUpdated(roomID)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) pruneOrRepairRoomTx(tx *sql.Tx, roomID string) (bool, error) {
	var memberCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ?`, roomID).Scan(&memberCount); err != nil {
		return false, err
	}
	if memberCount == 0 {
		_, err := tx.Exec(`DELETE FROM rooms WHERE id = ?`, roomID)
		return true, err
	}

	var adminCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND role IN ('owner', 'admin')`, roomID).Scan(&adminCount); err != nil {
		return false, err
	}
	if adminCount > 0 {
		return false, nil
	}

	_, err := tx.Exec(
		`UPDATE room_memberships
		 SET role = 'admin'
		 WHERE room_id = ?
		   AND user_id = (
		     SELECT user_id
		     FROM room_memberships
		     WHERE room_id = ? AND role = 'member'
		     ORDER BY RANDOM()
		     LIMIT 1
		   )`,
		roomID, roomID,
	)
	return false, err
}
