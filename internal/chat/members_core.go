package chat

import (
	"database/sql"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// memberCursor is a keyset pagination cursor over (joined_at, user_id). It is
// opaque to clients (base64 of "joinedAt:userID"). joined_at alone isn't
// unique, so user_id is the stable tiebreaker that guarantees a total order
// and no skipped/duplicated rows across pages.
func encodeMemberCursor(joinedAt int64, userID string) string {
	raw := strconv.FormatInt(joinedAt, 10) + ":" + userID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeMemberCursor(raw string) (joinedAt int64, userID string, ok bool) {
	if raw == "" {
		return 0, "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, "", false
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return 0, "", false
	}
	joinedAt, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil || parts[1] == "" {
		return 0, "", false
	}
	return joinedAt, parts[1], true
}

func (h *Handler) listMembers(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	viewerID := currentUserID(c)

	limit := parseLimit(c.Query("limit"), 50, 100)
	// Fetch one extra row to decide whether a further page exists.
	fetch := limit + 1

	var (
		rows *sql.Rows
		err  error
	)
	if curJoinedAt, curUserID, ok := decodeMemberCursor(c.Query("cursor")); ok {
		rows, err = h.DB.Query(
			`SELECT u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
			        rm.role, rm.text_muted_until, rm.joined_at, rm.room_display_name
			 FROM room_memberships rm
			 JOIN users u ON u.id = rm.user_id
			 WHERE rm.room_id = ?
			   AND (rm.joined_at > ? OR (rm.joined_at = ? AND rm.user_id > ?))
			 ORDER BY rm.joined_at ASC, rm.user_id ASC
			 LIMIT ?`,
			roomID, curJoinedAt, curJoinedAt, curUserID, fetch,
		)
	} else {
		rows, err = h.DB.Query(
			`SELECT u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
			        rm.role, rm.text_muted_until, rm.joined_at, rm.room_display_name
			 FROM room_memberships rm
			 JOIN users u ON u.id = rm.user_id
			 WHERE rm.room_id = ?
			 ORDER BY rm.joined_at ASC, rm.user_id ASC
			 LIMIT ?`,
			roomID, fetch,
		)
	}
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to list members")
		return
	}
	defer rows.Close()

	members := make([]currentMember, 0)
	rawJoinedAt := make([]int64, 0)
	for rows.Next() {
		var id, uid, username string
		var displayName, avatarURL, defaultAvatar sql.NullString
		var role string
		var textMutedUntil sql.NullInt64
		var joinedAt int64
		var roomDisplayName sql.NullString
		if err := rows.Scan(&id, &uid, &username, &displayName, &avatarURL, &defaultAvatar, &role, &textMutedUntil, &joinedAt, &roomDisplayName); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read members")
			return
		}
		var mutedUntil *string
		if textMutedUntil.Valid {
			v := formatMillis(textMutedUntil.Int64)
			mutedUntil = &v
		}
		user := summaryFromUserFields(id, uid, username, displayName, avatarURL, defaultAvatar)
		user.RoomDisplayName = nullableString(roomDisplayName)
		user.RoomRole = role
		isOnline := h.isUserOnlineForViewer(id, viewerID)
		user.IsOnline = &isOnline
		members = append(members, currentMember{
			User:           user,
			Role:           role,
			TextMutedUntil: mutedUntil,
			JoinedAt:       formatMillis(joinedAt),
		})
		rawJoinedAt = append(rawJoinedAt, joinedAt)
	}

	// More rows than the page size means there's a next page; trim the probe
	// row and hand back a cursor anchored on the last returned member.
	var nextCursor any
	if len(members) > limit {
		members = members[:limit]
		last := members[limit-1]
		nextCursor = encodeMemberCursor(rawJoinedAt[limit-1], last.User.ID)
	}
	c.JSON(http.StatusOK, gin.H{"members": members, "next_cursor": nextCursor})
}

func (h *Handler) joinRoom(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.roomExists(c, roomID) {
		return
	}
	var req joinRoomRequest
	if !h.bindOptionalJSON(c, &req) {
		return
	}
	userID := currentUserID(c)
	isSuperuser := h.isSuperuser(userID)
	if !isSuperuser && h.isRoomBlacklisted(roomID, userID) {
		h.jsonError(c, http.StatusNotFound, "not_found", "room not found")
		return
	}
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
		reason := cleanJoinRequestReason(req.Reason)
		_, err := h.DB.Exec(
			`INSERT INTO join_requests (id, room_id, user_id, status, reason, created_at, updated_at)
			 VALUES (?, ?, ?, 'pending', ?, ?, ?)
			 ON DUPLICATE KEY UPDATE
			   status = 'pending',
			   reason = VALUES(reason),
			   created_at = VALUES(created_at),
			   updated_at = VALUES(updated_at),
			   reviewer_user_id = NULL,
			   reviewed_at = NULL`,
			id, roomID, userID, reason, now, now,
		)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to create join request")
			return
		}
		var requestID, status string
		var createdAt int64
		_ = h.DB.QueryRow(`SELECT id, status, created_at FROM join_requests WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&requestID, &status, &createdAt)
		h.publishRoomApplicationsUpdated(userID)
		h.publishRoomJoinRequestsUpdated(roomID)
		c.JSON(http.StatusAccepted, gin.H{"join_request": gin.H{"id": requestID, "room_id": roomID, "status": status, "reason": reason, "created_at": formatMillis(createdAt)}})
		return
	}
	if alreadyMember == 0 && policy == "closed" && !isSuperuser {
		h.jsonError(c, http.StatusForbidden, "forbidden", "room is not accepting joins")
		return
	}
	if isSuperuser {
		detail, err := h.buildRoomDetail(roomID, userID)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room")
			return
		}
		if h.Bus != nil {
			h.Bus.AddUserRoomInterest(userID, roomID)
		}
		c.JSON(http.StatusOK, gin.H{"room": detail})
		return
	}

	tx, err := h.DB.Begin()
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to join room")
		return
	}
	defer tx.Rollback()
	now := nowMillis()
	if _, err := h.addRoomMemberTx(tx, roomID, userID, now); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to join room")
		return
	}
	if alreadyMember == 0 {
		if err := h.appendSystemMessageTx(tx, roomID, systemMessageSpec{
			Event:  systemEventRoomMemberJoined,
			UserID: userID,
		}); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to save room message")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to save room membership")
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
	if h.isSuperuser(userID) {
		if !h.roomIDExists(roomID) {
			h.jsonError(c, http.StatusNotFound, "not_found", "room not found")
			return
		}
		h.jsonError(c, http.StatusForbidden, "forbidden", "super user cannot leave rooms")
		return
	}
	if !h.isRoomMember(roomID, userID) {
		h.jsonError(c, http.StatusNotFound, "not_found", "room not found")
		return
	}
	var req leaveRoomRequest
	if !h.bindOptionalJSON(c, &req) {
		return
	}

	// If this user is the last member, leaving deletes the room. Require an
	// explicit confirm_delete_if_empty so the client can prompt first instead
	// of silently destroying the room.
	if !req.ConfirmDeleteIfEmpty {
		var memberCount int
		if err := h.DB.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ?`, roomID).Scan(&memberCount); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to leave room")
			return
		}
		if memberCount <= 1 {
			h.jsonError(c, http.StatusConflict, "confirmation_required", "leaving will delete this empty room; resend with confirm_delete_if_empty")
			return
		}
	}

	tx, err := h.DB.Begin()
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to leave room")
		return
	}
	defer tx.Rollback()

	liveRes, _ := tx.Exec(`DELETE FROM live_participants WHERE room_id = ? AND user_id = ?`, roomID, userID)
	leftLive := false
	if n, _ := liveRes.RowsAffected(); n > 0 {
		leftLive = true
	}
	if err := h.deleteRoomInviteHistoryForTargetTx(tx, roomID, userID); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to leave room")
		return
	}
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
	if !pruned {
		if err := h.appendSystemMessageTx(tx, roomID, systemMessageSpec{
			Event:  systemEventRoomMemberLeft,
			UserID: userID,
		}); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to save room message")
			return
		}
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
	h.publishPendingRoomInvitesUpdatedForInviter(roomID, userID)
	h.publishRoomInvitesUpdated(userID)
	if !pruned {
		h.publishRoomUpdated(roomID)
		if leftLive {
			h.PublishLiveSnapshot(roomID, "live_participant_left", map[string]any{"user_id": userID})
		}
	} else {
		h.publishPendingRoomInvitesUpdatedForRoom(roomID)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) addRoomMemberTx(tx *sql.Tx, roomID, userID string, joinedAt int64) (string, error) {
	role := "member"
	var ownerCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND role = 'owner'`, roomID).Scan(&ownerCount); err != nil {
		return "", err
	}
	if ownerCount == 0 {
		role = "owner"
	}
	res, err := tx.Exec(
		`INSERT IGNORE INTO room_memberships (room_id, user_id, role, joined_at)
		 VALUES (?, ?, ?, ?)`,
		roomID, userID, role, joinedAt,
	)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_ = tx.QueryRow(`SELECT role FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&role)
		return role, nil
	}
	if role == "owner" {
		if _, err := tx.Exec(`UPDATE rooms SET created_by_user_id = ?, updated_at = ? WHERE id = ?`, userID, joinedAt, roomID); err != nil {
			return "", err
		}
	}
	return role, nil
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

	var ownerCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND role = 'owner'`, roomID).Scan(&ownerCount); err != nil {
		return false, err
	}
	if ownerCount > 0 {
		return false, nil
	}

	var nextOwner string
	if err := tx.QueryRow(
		`SELECT rm.user_id
		 FROM room_memberships rm
		 LEFT JOIN live_participants lp ON lp.room_id = rm.room_id AND lp.user_id = rm.user_id
		 WHERE rm.room_id = ?
		 ORDER BY
		   CASE WHEN rm.role = 'admin' THEN 0 ELSE 1 END,
		   CASE WHEN lp.user_id IS NOT NULL THEN 0 ELSE 1 END,
		   RAND()
		 LIMIT 1`,
		roomID,
	).Scan(&nextOwner); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE room_memberships SET role = 'owner' WHERE room_id = ? AND user_id = ?`, roomID, nextOwner); err != nil {
		return false, err
	}
	_, err := tx.Exec(
		`UPDATE rooms SET created_by_user_id = ?, updated_at = ? WHERE id = ?`,
		nextOwner, nowMillis(), roomID,
	)
	return false, err
}
