package chat

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (h *Handler) getMemberProfile(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	var id, uid, username, role string
	var displayName, avatarURL, defaultAvatar, roomDisplayName, roomAvatarURL, roomDefaultAvatar sql.NullString
	var textMutedUntil sql.NullInt64
	var joinedAt int64
	err := h.DB.QueryRow(
		`SELECT u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
		        rm.room_display_name, rm.room_avatar_url, rm.room_default_avatar_key,
		        rm.role, rm.text_muted_until, rm.joined_at
		 FROM room_memberships rm JOIN users u ON u.id = rm.user_id
		 WHERE rm.room_id = ? AND rm.user_id = ?`,
		roomID, c.Param("user_id"),
	).Scan(&id, &uid, &username, &displayName, &avatarURL, &defaultAvatar, &roomDisplayName, &roomAvatarURL, &roomDefaultAvatar, &role, &textMutedUntil, &joinedAt)
	if err != nil {
		h.jsonError(c, http.StatusNotFound, "not_found", "member not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"profile": gin.H{
		"user":                    summaryFromUserFields(id, uid, username, displayName, avatarURL, defaultAvatar),
		"room_display_name":       nullableString(roomDisplayName),
		"room_avatar_url":         nullableString(roomAvatarURL),
		"room_default_avatar_key": nullableString(roomDefaultAvatar),
		"role":                    role,
		"text_muted_until":        nullableMillis(textMutedUntil),
		"joined_at":               formatMillis(joinedAt),
	}})
}

func (h *Handler) getMyRoomSettings(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireMember(c, roomID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": h.myRoomSettingsPayload(roomID, currentUserID(c))})
}

func (h *Handler) updateMyRoomSettings(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireMember(c, roomID) {
		return
	}
	var req myRoomSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	sets := []string{}
	args := []any{}
	if req.RemarkName != nil {
		sets = append(sets, "remark_name = ?")
		args = append(args, emptyToNil(*req.RemarkName))
	}
	if req.RoomDisplayName != nil {
		sets = append(sets, "room_display_name = ?")
		args = append(args, emptyToNil(*req.RoomDisplayName))
	}
	if req.NotificationLevel != nil {
		if !allowed(*req.NotificationLevel, "all", "mentions", "muted") {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid notification_level")
			return
		}
		sets = append(sets, "notification_level = ?")
		args = append(args, *req.NotificationLevel)
	}
	if req.RoomAvatarAssetID != nil {
		var url sql.NullString
		_ = h.DB.QueryRow(`SELECT url FROM assets WHERE id = ? AND owner_user_id = ?`, *req.RoomAvatarAssetID, currentUserID(c)).Scan(&url)
		sets = append(sets, "room_avatar_url = ?", "room_default_avatar_key = ?")
		if url.Valid {
			args = append(args, url.String, nil)
		} else {
			args = append(args, nil, nil)
		}
	}
	if len(sets) == 0 {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "at least one setting is required")
		return
	}
	args = append(args, roomID, currentUserID(c))
	if _, err := h.DB.Exec(`UPDATE room_memberships SET `+strings.Join(sets, ", ")+` WHERE room_id = ? AND user_id = ?`, args...); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "update room settings failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": h.myRoomSettingsPayload(roomID, currentUserID(c))})
}

func (h *Handler) getRoomSettings(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.isAdmin(roomID, currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": h.roomSettingsPayload(roomID)})
}

func (h *Handler) updateRoomSettings(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.isAdmin(roomID, currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	var req roomSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	sets := []string{"updated_at = ?"}
	args := []any{nowMillis()}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "name is required")
			return
		}
		sets = append(sets, "name = ?")
		args = append(args, name)
	}
	if req.Visibility != nil {
		if !allowed(*req.Visibility, "public", "private") {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid visibility")
			return
		}
		sets = append(sets, "visibility = ?")
		args = append(args, *req.Visibility)
	}
	if req.JoinPolicy != nil {
		if !allowed(*req.JoinPolicy, "open", "approval_required", "closed") {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid join_policy")
			return
		}
		sets = append(sets, "join_policy = ?")
		args = append(args, *req.JoinPolicy)
	}
	if req.AIVoiceAnnounceEnabled != nil {
		sets = append(sets, "ai_voice_announce_enabled = ?")
		args = append(args, boolToInt(*req.AIVoiceAnnounceEnabled))
	}
	if req.MessageRecallPolicy != nil {
		if !allowed(*req.MessageRecallPolicy, "disabled", "admin_approval", "time_limited") {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid message_recall_policy")
			return
		}
		sets = append(sets, "message_recall_policy = ?")
		args = append(args, *req.MessageRecallPolicy)
	}
	if req.MessageRecallWindowSeconds != nil {
		sets = append(sets, "message_recall_window_seconds = ?")
		args = append(args, *req.MessageRecallWindowSeconds)
	}
	args = append(args, roomID)
	if _, err := h.DB.Exec(`UPDATE rooms SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "update room settings failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": h.roomSettingsPayload(roomID)})
}

func (h *Handler) inviteMember(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.isAdmin(roomID, currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	var req userIDRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "user_id is required")
		return
	}
	now := nowMillis()
	_, err := h.DB.Exec(
		`INSERT INTO room_memberships (room_id, user_id, role, joined_at)
		 VALUES (?, ?, 'member', ?)
		 ON CONFLICT(room_id, user_id) DO NOTHING`,
		roomID, req.UserID, now,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "invite member failed")
		return
	}
	member := h.memberPayload(roomID, req.UserID)
	c.JSON(http.StatusCreated, gin.H{"member": member})
}

func (h *Handler) removeMember(c *gin.Context) {
	roomID := c.Param("room_id")
	actorID := currentUserID(c)
	if !h.isAdmin(roomID, actorID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	if !h.bindOptionalJSON(c, nil) {
		return
	}
	targetID := c.Param("user_id")
	if h.isProtectedSuperuserTarget(actorID, targetID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot manage super user")
		return
	}
	var role string
	_ = h.DB.QueryRow(`SELECT role FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, targetID).Scan(&role)
	if role == "owner" {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot remove room owner")
		return
	}
	tx, err := h.DB.Begin()
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "remove member failed")
		return
	}
	defer tx.Rollback()
	_, _ = tx.Exec(`DELETE FROM live_participants WHERE room_id = ? AND user_id = ?`, roomID, targetID)
	res, err := tx.Exec(`DELETE FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, targetID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "remove member failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.jsonError(c, http.StatusNotFound, "not_found", "member not found")
		return
	}
	if _, err := h.pruneOrRepairRoomTx(tx, roomID); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "repair room admins failed")
		return
	}
	if err := tx.Commit(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "save membership failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) updateMemberRole(c *gin.Context) {
	roomID := c.Param("room_id")
	actorID := currentUserID(c)
	if !h.isAdmin(roomID, actorID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	var req roleRequest
	if err := c.ShouldBindJSON(&req); err != nil || !allowed(req.Role, "admin", "member") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "role must be admin or member")
		return
	}
	targetID := c.Param("user_id")
	if h.isProtectedSuperuserTarget(actorID, targetID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot manage super user")
		return
	}
	var currentRole string
	_ = h.DB.QueryRow(`SELECT role FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, targetID).Scan(&currentRole)
	if currentRole == "owner" {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot change owner role")
		return
	}
	tx, err := h.DB.Begin()
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "update role failed")
		return
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE room_memberships SET role = ? WHERE room_id = ? AND user_id = ?`, req.Role, roomID, targetID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "update role failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.jsonError(c, http.StatusNotFound, "not_found", "member not found")
		return
	}
	if _, err := h.pruneOrRepairRoomTx(tx, roomID); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "repair room admins failed")
		return
	}
	if err := tx.Commit(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "save membership failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"member": h.memberPayload(roomID, targetID)})
}

func (h *Handler) textMuteMember(c *gin.Context) {
	h.setTextMute(c, true)
}

func (h *Handler) textUnmuteMember(c *gin.Context) {
	h.setTextMute(c, false)
}

func (h *Handler) setTextMute(c *gin.Context, mute bool) {
	roomID := c.Param("room_id")
	actorID := currentUserID(c)
	if !h.isAdmin(roomID, actorID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	if h.isProtectedSuperuserTarget(actorID, c.Param("user_id")) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot manage super user")
		return
	}
	var mutedUntil any
	if mute {
		var req muteRequest
		if !h.bindOptionalJSON(c, &req) {
			return
		}
		if req.DurationSeconds == nil {
			mutedUntil = int64(0)
		} else {
			mutedUntil = nowMillis() + (*req.DurationSeconds * 1000)
		}
	} else {
		var req reasonRequest
		if !h.bindOptionalJSON(c, &req) {
			return
		}
	}
	res, err := h.DB.Exec(`UPDATE room_memberships SET text_muted_until = ? WHERE room_id = ? AND user_id = ?`, mutedUntil, roomID, c.Param("user_id"))
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "text mute failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.jsonError(c, http.StatusNotFound, "not_found", "member not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"member": gin.H{"user_id": c.Param("user_id"), "text_muted_until": h.textMutedUntil(roomID, c.Param("user_id"))}})
}

func (h *Handler) listJoinRequests(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.isAdmin(roomID, currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	status := c.Query("status")
	if status == "" {
		status = "pending"
	}
	rows, err := h.DB.Query(
		`SELECT jr.id, jr.status, jr.created_at, u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key
		 FROM join_requests jr JOIN users u ON u.id = jr.user_id
		 WHERE jr.room_id = ? AND jr.status = ?
		 ORDER BY jr.created_at ASC`,
		roomID, status,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "list join requests failed")
		return
	}
	defer rows.Close()
	requests := make([]gin.H, 0)
	for rows.Next() {
		request, err := scanJoinRequest(rows)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "read join request failed")
			return
		}
		requests = append(requests, request)
	}
	c.JSON(http.StatusOK, gin.H{"requests": requests, "next_cursor": nil})
}

func (h *Handler) reviewJoinRequest(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.isAdmin(roomID, currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	var req decisionRequest
	if err := c.ShouldBindJSON(&req); err != nil || !allowed(req.Decision, "approve", "reject") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "decision must be approve or reject")
		return
	}
	var userID string
	var status string
	err := h.DB.QueryRow(`SELECT user_id, status FROM join_requests WHERE id = ? AND room_id = ?`, c.Param("request_id"), roomID).Scan(&userID, &status)
	if err != nil {
		h.jsonError(c, http.StatusNotFound, "not_found", "join request not found")
		return
	}
	newStatus := "rejected"
	if req.Decision == "approve" {
		newStatus = "approved"
		_, _ = h.DB.Exec(
			`INSERT INTO room_memberships (room_id, user_id, role, joined_at)
			 VALUES (?, ?, 'member', ?)
			 ON CONFLICT(room_id, user_id) DO NOTHING`,
			roomID, userID, nowMillis(),
		)
	}
	_, _ = h.DB.Exec(`UPDATE join_requests SET status = ?, updated_at = ? WHERE id = ?`, newStatus, nowMillis(), c.Param("request_id"))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) deleteRoom(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.roomExists(c, roomID) {
		return
	}
	var role string
	userID := currentUserID(c)
	_ = h.DB.QueryRow(`SELECT role FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&role)
	if role != "owner" && !h.isSuperuser(userID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "owner required")
		return
	}
	var req struct {
		ConfirmRID string `json:"confirm_rid"`
	}
	_ = c.ShouldBindJSON(&req)
	var rid string
	_ = h.DB.QueryRow(`SELECT rid FROM rooms WHERE id = ?`, roomID).Scan(&rid)
	if req.ConfirmRID != "" && req.ConfirmRID != rid {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "confirm_rid mismatch")
		return
	}
	_, _ = h.DB.Exec(`DELETE FROM rooms WHERE id = ?`, roomID)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) roomSettingsPayload(roomID string) gin.H {
	var rid, name, defaultAvatar, visibility, joinPolicy, recallPolicy string
	var avatarAssetID, avatarURL sql.NullString
	var ai int
	var recallWindow sql.NullInt64
	var createdAt, updatedAt int64
	_ = h.DB.QueryRow(
		`SELECT rid, name, avatar_asset_id, avatar_url, default_avatar_key, visibility, join_policy,
		        ai_voice_announce_enabled, message_recall_policy, message_recall_window_seconds,
		        created_at, updated_at
		 FROM rooms WHERE id = ?`,
		roomID,
	).Scan(&rid, &name, &avatarAssetID, &avatarURL, &defaultAvatar, &visibility, &joinPolicy, &ai, &recallPolicy, &recallWindow, &createdAt, &updatedAt)
	return gin.H{
		"id": roomID, "rid": rid, "name": name, "avatar_asset_id": nullableString(avatarAssetID),
		"avatar_url": nullableString(avatarURL), "default_avatar_key": defaultAvatar,
		"visibility": visibility, "join_policy": joinPolicy, "ai_voice_announce_enabled": ai != 0,
		"message_recall_policy": recallPolicy, "message_recall_window_seconds": nullableInt64(recallWindow),
		"created_at": formatMillis(createdAt), "updated_at": formatMillis(updatedAt),
	}
}

func (h *Handler) myRoomSettingsPayload(roomID, userID string) gin.H {
	var remark, display, avatarURL, defaultAvatar sql.NullString
	var notification string
	_ = h.DB.QueryRow(
		`SELECT remark_name, room_display_name, room_avatar_url, room_default_avatar_key, notification_level
		 FROM room_memberships WHERE room_id = ? AND user_id = ?`,
		roomID, userID,
	).Scan(&remark, &display, &avatarURL, &defaultAvatar, &notification)
	return gin.H{
		"remark_name": nullableString(remark), "room_display_name": nullableString(display),
		"room_avatar_asset_id": nil, "room_avatar_url": nullableString(avatarURL),
		"room_default_avatar_key": nullableString(defaultAvatar), "notification_level": notification,
	}
}

func (h *Handler) memberPayload(roomID, userID string) gin.H {
	var id, uid, username, role string
	var displayName, avatarURL, defaultAvatar sql.NullString
	var joinedAt int64
	_ = h.DB.QueryRow(
		`SELECT u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key, rm.role, rm.joined_at
		 FROM room_memberships rm JOIN users u ON u.id = rm.user_id
		 WHERE rm.room_id = ? AND rm.user_id = ?`,
		roomID, userID,
	).Scan(&id, &uid, &username, &displayName, &avatarURL, &defaultAvatar, &role, &joinedAt)
	return gin.H{"user": summaryFromUserFields(id, uid, username, displayName, avatarURL, defaultAvatar), "role": role, "joined_at": formatMillis(joinedAt)}
}

func scanJoinRequest(rows *sql.Rows) (gin.H, error) {
	var requestID, status, id, uid, username string
	var displayName, avatarURL, defaultAvatar sql.NullString
	var createdAt int64
	if err := rows.Scan(&requestID, &status, &createdAt, &id, &uid, &username, &displayName, &avatarURL, &defaultAvatar); err != nil {
		return nil, err
	}
	return gin.H{
		"id": requestID, "status": status, "created_at": formatMillis(createdAt),
		"user": summaryFromUserFields(id, uid, username, displayName, avatarURL, defaultAvatar),
	}, nil
}

func (h *Handler) textMutedUntil(roomID, userID string) *string {
	var muted sql.NullInt64
	_ = h.DB.QueryRow(`SELECT text_muted_until FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&muted)
	return nullableMillis(muted)
}

func nullableMillis(value sql.NullInt64) *string {
	if !value.Valid {
		return nil
	}
	if value.Int64 == 0 {
		v := "permanent"
		return &v
	}
	v := formatMillis(value.Int64)
	return &v
}

func emptyToNil(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}
