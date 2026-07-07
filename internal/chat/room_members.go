package chat

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (h *Handler) getMemberProfile(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	viewerID := currentUserID(c)
	targetID := c.Param("user_id")
	var id, role string
	var roomDisplayName sql.NullString
	var textMutedUntil sql.NullInt64
	var joinedAt int64
	err := h.DB.QueryRow(
		`SELECT u.id,
		        rm.room_display_name,
		        rm.role, rm.text_muted_until, rm.joined_at
		 FROM room_memberships rm JOIN users u ON u.id = rm.user_id
		 WHERE rm.room_id = ? AND rm.user_id = ?`,
		roomID, targetID,
	).Scan(&id, &roomDisplayName, &role, &textMutedUntil, &joinedAt)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read member profile")
			return
		}
		user, userErr := h.profileUserSummary(targetID, viewerID)
		if userErr != nil || !user.IsSuperuser || !h.roomIDExists(roomID) {
			h.jsonError(c, http.StatusNotFound, "not_found", "member not found")
			return
		}
		c.JSON(http.StatusOK, gin.H{"profile": gin.H{
			"user":              user,
			"room_display_name": nil,
			"role":              "superuser",
			"text_muted_until":  nil,
			"joined_at":         formatMillis(nowMillis()),
		}})
		return
	}
	user, err := h.profileUserSummary(id, viewerID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read member profile")
		return
	}
	c.JSON(http.StatusOK, gin.H{"profile": gin.H{
		"user":              user,
		"room_display_name": nullableString(roomDisplayName),
		"role":              role,
		"text_muted_until":  nullableMillis(textMutedUntil),
		"joined_at":         formatMillis(joinedAt),
	}})
}

func (h *Handler) getUserProfile(c *gin.Context) {
	user, err := h.profileUserSummary(c.Param("user_id"), currentUserID(c))
	if errors.Is(err, sql.ErrNoRows) {
		h.jsonError(c, http.StatusNotFound, "not_found", "user not found")
		return
	}
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read user profile")
		return
	}
	c.JSON(http.StatusOK, gin.H{"profile": gin.H{"user": user}})
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
	var req myRoomSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	userID := currentUserID(c)
	if !h.isRoomMember(roomID, userID) {
		if h.isSuperuser(userID) && h.roomIDExists(roomID) {
			detail, err := h.buildRoomDetail(roomID, userID)
			if err != nil {
				h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room")
				return
			}
			c.JSON(http.StatusOK, gin.H{"settings": h.myRoomSettingsPayload(roomID, userID), "room": detail})
			return
		}
		h.jsonError(c, http.StatusNotFound, "not_found", "room not found")
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
	notificationPolicy := req.NotificationLevel
	notificationField := "notification_level"
	if req.NotificationPolicy != nil {
		notificationPolicy = req.NotificationPolicy
		notificationField = "notification_policy"
	}
	if notificationPolicy != nil {
		normalizedNotificationPolicy, ok := normalizeRoomNotificationPolicy(*notificationPolicy)
		if !ok {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid "+notificationField)
			return
		}
		sets = append(sets, "notification_level = ?")
		args = append(args, normalizedNotificationPolicy)
	}
	isPinned := req.IsPinned
	if isPinned == nil {
		isPinned = req.Pinned
	}
	if isPinned != nil {
		sets = append(sets, "is_pinned = ?")
		args = append(args, boolToInt(*isPinned))
	}
	if len(sets) == 0 {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "at least one setting is required")
		return
	}
	args = append(args, roomID, userID)
	if _, err := h.DB.Exec(`UPDATE room_memberships SET `+strings.Join(sets, ", ")+` WHERE room_id = ? AND user_id = ?`, args...); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "update room settings failed")
		return
	}
	detail, err := h.buildRoomDetail(roomID, userID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room")
		return
	}
	h.publishRoomToUser(userID, roomID, "room_updated")
	c.JSON(http.StatusOK, gin.H{"settings": h.myRoomSettingsPayload(roomID, userID), "room": detail})
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
	userID := currentUserID(c)
	if !h.isAdmin(roomID, userID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	var req roomSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	oldName, oldDescription, oldJoinPolicy := "", "", ""
	if req.Name != nil || req.Description != nil || req.JoinPolicy != nil {
		_ = h.DB.QueryRow(
			`SELECT name, description, join_policy FROM rooms WHERE id = ?`,
			roomID,
		).Scan(&oldName, &oldDescription, &oldJoinPolicy)
	}
	newName, newDescription := "", ""
	nameChanged := false
	descriptionChanged := false
	joinPolicyClosedStateChanged := false
	updatedAt := nowMillis()
	sets := []string{"updated_at = ?"}
	args := []any{updatedAt}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "name is required")
			return
		}
		newName = name
		nameChanged = name != oldName
		sets = append(sets, "name = ?")
		args = append(args, name)
	}
	if req.Description != nil {
		description := strings.TrimSpace(*req.Description)
		if len([]rune(description)) > 500 {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "description must be at most 500 characters")
			return
		}
		newDescription = description
		descriptionChanged = description != oldDescription
		sets = append(sets, "description = ?")
		args = append(args, description)
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
		joinPolicyClosedStateChanged = roomJoinPolicyClosed(oldJoinPolicy) != roomJoinPolicyClosed(*req.JoinPolicy)
		sets = append(sets, "join_policy = ?")
		args = append(args, *req.JoinPolicy)
	}
	aiVoiceAnnounceEnabled := req.AIVoiceAnnounceEnabled
	if req.AIVoiceAnnouncementsEnabled != nil {
		aiVoiceAnnounceEnabled = req.AIVoiceAnnouncementsEnabled
	}
	if aiVoiceAnnounceEnabled != nil {
		sets = append(sets, "ai_voice_announce_enabled = ?")
		args = append(args, boolToInt(*aiVoiceAnnounceEnabled))
	}
	if req.AvatarAssetID != nil {
		assetID := strings.TrimSpace(*req.AvatarAssetID)
		if assetID == "" {
			sets = append(sets, "avatar_asset_id = ?", "avatar_url = ?")
			args = append(args, nil, nil)
		} else {
			var filename string
			if err := h.DB.QueryRow(`SELECT filename FROM assets WHERE id = ? AND owner_user_id = ?`, assetID, userID).Scan(&filename); err != nil {
				h.jsonError(c, http.StatusBadRequest, "validation_failed", "avatar asset not found")
				return
			}
			sets = append(sets, "avatar_asset_id = ?", "avatar_url = ?")
			args = append(args, assetID, h.assetStore().PublicURL(h.assetStore().ObjectKey(assetID, filename), assetID, filename))
		}
	}
	if req.DefaultAvatarKey != nil {
		key := strings.TrimSpace(*req.DefaultAvatarKey)
		if key == "" {
			key = defaultRoomAvatar(roomID)
		}
		sets = append(sets, "default_avatar_key = ?")
		args = append(args, key)
		if req.AvatarAssetID == nil && key != h.currentRoomAvatarKey(roomID) {
			sets = append(sets, "avatar_asset_id = ?", "avatar_url = ?")
			args = append(args, nil, nil)
		}
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
	tx, err := h.DB.Begin()
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "update room settings failed")
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE rooms SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "update room settings failed")
		return
	}
	if nameChanged {
		if err := h.appendSystemMessageTx(tx, roomID, systemMessageSpec{
			Event:     systemEventRoomNameChanged,
			UserID:    userID,
			ActorID:   userID,
			OldValue:  oldName,
			NewValue:  newName,
			CreatedAt: updatedAt,
		}); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "append system message failed")
			return
		}
	}
	if descriptionChanged {
		createdAt := updatedAt
		if nameChanged {
			createdAt++
		}
		if err := h.appendSystemMessageTx(tx, roomID, systemMessageSpec{
			Event:     systemEventRoomBioChanged,
			UserID:    userID,
			ActorID:   userID,
			OldValue:  oldDescription,
			NewValue:  newDescription,
			CreatedAt: createdAt,
		}); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "append system message failed")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "update room settings failed")
		return
	}
	// Name / avatar / visibility / join_policy etc. all live in the room-list
	// snapshot, so every member's list entry needs to be refreshed.
	h.publishRoomUpdated(roomID)
	if joinPolicyClosedStateChanged {
		h.publishPendingRoomInvitesUpdatedForRoom(roomID)
	}
	detail, err := h.buildRoomDetail(roomID, currentUserID(c))
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room")
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": h.roomSettingsPayload(roomID), "room": detail})
}

func (h *Handler) inviteMember(c *gin.Context) {
	roomID := c.Param("room_id")
	inviterID := currentUserID(c)
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	var req userIDRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "user_id is required")
		return
	}
	var joinPolicy string
	if err := h.DB.QueryRow(`SELECT join_policy FROM rooms WHERE id = ?`, roomID).Scan(&joinPolicy); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room")
		return
	}
	if joinPolicy == "closed" {
		h.jsonError(c, http.StatusForbidden, "forbidden", "room invitations are disabled")
		return
	}
	if req.UserID == inviterID {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "cannot invite yourself")
		return
	}
	var targetExists int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ? AND status = 'active'`, req.UserID).Scan(&targetExists); err != nil || targetExists == 0 {
		h.jsonError(c, http.StatusNotFound, "not_found", "user not found")
		return
	}
	var alreadyMember int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, req.UserID).Scan(&alreadyMember)
	if alreadyMember > 0 {
		h.jsonError(c, http.StatusConflict, "already_member", "user is already a room member")
		return
	}
	if h.isRoomBlacklisted(roomID, req.UserID) {
		h.jsonError(c, http.StatusForbidden, "blocked", "user is blocked from this room")
		return
	}
	var existingPendingInviteID string
	err := h.DB.QueryRow(
		`SELECT id
		 FROM room_invites
		 WHERE room_id = ? AND target_user_id = ? AND inviter_user_id = ? AND status = 'pending'`,
		roomID, req.UserID, inviterID,
	).Scan(&existingPendingInviteID)
	if err == nil {
		c.JSON(http.StatusOK, gin.H{"invite": h.roomInvitePayload(existingPendingInviteID, req.UserID)})
		return
	}
	if err != sql.ErrNoRows {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "read room invite failed")
		return
	}
	now := nowMillis()
	id := newID("rinv")
	_, err = h.DB.Exec(
		`INSERT INTO room_invites (
		   id, room_id, inviter_user_id, target_user_id, status, created_at, updated_at,
		   room_rid, room_name, room_avatar_url, room_default_avatar_key, room_visibility, room_join_policy
		 )
		 SELECT ?, r.id, ?, ?, 'pending', ?, ?,
		        r.rid, r.name, r.avatar_url, r.default_avatar_key, r.visibility, r.join_policy
		 FROM rooms r
		 WHERE r.id = ?
		 ON DUPLICATE KEY UPDATE
		   status = 'pending',
		   created_at = VALUES(created_at),
		   updated_at = VALUES(updated_at),
		   room_rid = VALUES(room_rid),
		   room_name = VALUES(room_name),
		   room_avatar_url = VALUES(room_avatar_url),
		   room_default_avatar_key = VALUES(room_default_avatar_key),
		   room_visibility = VALUES(room_visibility),
		   room_join_policy = VALUES(room_join_policy)`,
		id, inviterID, req.UserID, now, now, roomID,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "invite member failed")
		return
	}
	var inviteID string
	if err := h.DB.QueryRow(`SELECT id FROM room_invites WHERE room_id = ? AND target_user_id = ? AND inviter_user_id = ?`, roomID, req.UserID, inviterID).Scan(&inviteID); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "read room invite failed")
		return
	}
	h.publishRoomInvitesUpdated(req.UserID)
	c.JSON(http.StatusCreated, gin.H{"invite": h.roomInvitePayload(inviteID, req.UserID)})
}

func (h *Handler) listRoomBlacklist(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.isAdmin(roomID, currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	rows, err := h.DB.Query(
		`SELECT rb.user_id, rb.blocked_by_user_id, rb.created_at,
		        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
		        bu.id, bu.uid, bu.username, bu.display_name, bu.avatar_url, bu.default_avatar_key
		 FROM room_blacklist rb
		 JOIN users u ON u.id = rb.user_id
		 LEFT JOIN users bu ON bu.id = rb.blocked_by_user_id
		 WHERE rb.room_id = ?
		 ORDER BY rb.created_at DESC, lower(u.username) ASC`,
		roomID,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "list room blacklist failed")
		return
	}
	defer rows.Close()

	items := make([]gin.H, 0)
	for rows.Next() {
		entry, err := scanRoomBlacklistEntry(rows)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "read room blacklist failed")
			return
		}
		items = append(items, entry)
	}
	c.JSON(http.StatusOK, gin.H{"blacklist": items, "items": items, "next_cursor": nil})
}

func (h *Handler) blockRoomUser(c *gin.Context) {
	roomID := c.Param("room_id")
	actorID := currentUserID(c)
	if !h.isAdmin(roomID, actorID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	var req userIDRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "user_id is required")
		return
	}
	var isSuperuser int
	if err := h.DB.QueryRow(`SELECT is_superuser FROM users WHERE id = ? AND status = 'active'`, req.UserID).Scan(&isSuperuser); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			h.jsonError(c, http.StatusNotFound, "not_found", "user not found")
			return
		}
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read user")
		return
	}
	if isSuperuser != 0 {
		h.jsonError(c, http.StatusForbidden, "protected_user", "super user cannot be blocked")
		return
	}
	if h.isRoomMember(roomID, req.UserID) {
		h.jsonError(c, http.StatusConflict, "room_member", "room members cannot be blocked")
		return
	}

	now := nowMillis()
	_, err := h.DB.Exec(
		`INSERT INTO room_blacklist (room_id, user_id, blocked_by_user_id, created_at)
		 VALUES (?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		   blocked_by_user_id = VALUES(blocked_by_user_id),
		   created_at = VALUES(created_at)`,
		roomID, req.UserID, actorID, now,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "block room user failed")
		return
	}
	entry, err := h.roomBlacklistEntryPayload(roomID, req.UserID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "read room blacklist failed")
		return
	}
	h.publishRoomInvitesUpdated(req.UserID)
	h.publishRoomJoinRequestsUpdated(roomID)
	c.JSON(http.StatusCreated, gin.H{"entry": entry})
}

func (h *Handler) unblockRoomUser(c *gin.Context) {
	roomID := c.Param("room_id")
	userID := c.Param("user_id")
	if !h.isAdmin(roomID, currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	if _, err := h.DB.Exec(`DELETE FROM room_blacklist WHERE room_id = ? AND user_id = ?`, roomID, userID); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "unblock room user failed")
		return
	}
	h.publishRoomInvitesUpdated(userID)
	h.publishRoomJoinRequestsUpdated(roomID)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) listRoomInvites(c *gin.Context) {
	userID := currentUserID(c)
	status := c.Query("status")
	if status == "" {
		status = "pending"
	}
	if status != "all" && !allowed(status, "pending", "accepted", "rejected") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "status must be pending, accepted, rejected, or all")
		return
	}
	query := `SELECT id
		 FROM room_invites
		 WHERE target_user_id = ?`
	args := []any{userID}
	if status != "all" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` AND NOT (
		status = 'pending'
		AND EXISTS (
			SELECT 1
			FROM room_blacklist rb
			WHERE rb.room_id = room_invites.room_id
			  AND rb.user_id = room_invites.target_user_id
		)
	)`
	query += ` AND NOT (
		status = 'pending'
		AND EXISTS (
			SELECT 1
			FROM rooms r
			WHERE r.id = room_invites.room_id
			  AND r.join_policy = 'closed'
		)
	)`
	query += ` ORDER BY CASE WHEN status = 'pending' THEN 0 ELSE 1 END, created_at DESC`
	rows, err := h.DB.Query(query, args...)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "list room invites failed")
		return
	}
	defer rows.Close()

	invites := make([]gin.H, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "read room invite failed")
			return
		}
		invites = append(invites, h.roomInvitePayload(id, userID))
	}
	c.JSON(http.StatusOK, gin.H{"invites": invites, "next_cursor": nil})
}

func (h *Handler) listRoomApplications(c *gin.Context) {
	userID := currentUserID(c)
	status := c.Query("status")
	if status == "" {
		status = "pending"
	}
	if status != "all" && !allowed(status, "pending", "approved", "rejected", "withdrawn") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "status must be pending, approved, rejected, withdrawn, or all")
		return
	}
	query := `SELECT id
		 FROM join_requests
		 WHERE user_id = ?`
	args := []any{userID}
	if status != "all" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY CASE WHEN status = 'pending' THEN 0 ELSE 1 END, created_at DESC`
	rows, err := h.DB.Query(query, args...)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "list room applications failed")
		return
	}
	defer rows.Close()

	applications := make([]gin.H, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "read room application failed")
			return
		}
		applications = append(applications, h.roomApplicationPayload(id, userID))
	}
	c.JSON(http.StatusOK, gin.H{"applications": applications, "next_cursor": nil})
}

func (h *Handler) withdrawRoomApplication(c *gin.Context) {
	var req decisionRequest
	if !h.bindOptionalJSON(c, &req) {
		return
	}
	if req.Decision != "" && req.Decision != "withdraw" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "decision must be withdraw")
		return
	}
	userID := currentUserID(c)
	requestID := c.Param("request_id")
	var roomID, status string
	err := h.DB.QueryRow(`SELECT room_id, status FROM join_requests WHERE id = ? AND user_id = ?`, requestID, userID).Scan(&roomID, &status)
	if err != nil {
		h.jsonError(c, http.StatusNotFound, "not_found", "room application not found")
		return
	}
	if status != "pending" {
		h.jsonError(c, http.StatusConflict, "not_pending", "room application is not pending")
		return
	}
	_, err = h.DB.Exec(
		`UPDATE join_requests
		 SET status = 'withdrawn', updated_at = ?, reviewer_user_id = NULL, reviewed_at = NULL
		 WHERE id = ?`,
		nowMillis(), requestID,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "withdraw room application failed")
		return
	}
	h.publishRoomApplicationsUpdated(userID)
	h.publishRoomJoinRequestsUpdated(roomID)
	c.JSON(http.StatusOK, gin.H{"ok": true, "application": h.roomApplicationPayload(requestID, userID)})
}

func (h *Handler) reviewRoomInvite(c *gin.Context) {
	var req decisionRequest
	if err := c.ShouldBindJSON(&req); err != nil || !allowed(req.Decision, "accept", "approve", "reject", "decline") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "decision must be accept or reject")
		return
	}
	userID := currentUserID(c)
	inviteID := c.Param("invite_id")
	var roomID, status, inviterID string
	err := h.DB.QueryRow(`SELECT room_id, status, inviter_user_id FROM room_invites WHERE id = ? AND target_user_id = ?`, inviteID, userID).Scan(&roomID, &status, &inviterID)
	if err != nil {
		h.jsonError(c, http.StatusNotFound, "not_found", "room invite not found")
		return
	}
	if status != "pending" {
		h.jsonError(c, http.StatusConflict, "not_pending", "room invite is not pending")
		return
	}
	if reason := h.roomInviteInvalidReason(roomID, inviterID, userID); reason != "" {
		h.jsonError(c, http.StatusConflict, "invalid_invite", "room invite is no longer valid")
		return
	}

	accept := req.Decision == "accept" || req.Decision == "approve"
	if !accept {
		_, _ = h.DB.Exec(`UPDATE room_invites SET status = 'rejected', updated_at = ? WHERE id = ?`, nowMillis(), inviteID)
		h.publishRoomInvitesUpdated(userID)
		c.JSON(http.StatusOK, gin.H{"ok": true, "invite": h.roomInvitePayload(inviteID, userID)})
		return
	}

	hasPrivilegedInvite := h.hasPrivilegedPendingRoomInvite(roomID, userID)

	var alreadyMember int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&alreadyMember)
	if alreadyMember > 0 {
		now := nowMillis()
		_, _ = h.DB.Exec(`UPDATE room_invites SET status = 'accepted', updated_at = ? WHERE room_id = ? AND target_user_id = ? AND status = 'pending'`, now, roomID, userID)
		joinRequestUpdated := false
		if res, err := h.DB.Exec(
			`UPDATE join_requests
			 SET status = 'approved', updated_at = ?, reviewer_user_id = NULL, reviewed_at = ?
			 WHERE room_id = ? AND user_id = ? AND status = 'pending'`,
			now, now, roomID, userID,
		); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "save room invite decision failed")
			return
		} else if n, _ := res.RowsAffected(); n > 0 {
			joinRequestUpdated = true
		}
		detail, err := h.buildRoomDetail(roomID, userID)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room")
			return
		}
		h.publishRoomInvitesUpdated(userID)
		if joinRequestUpdated {
			h.publishRoomApplicationsUpdated(userID)
			h.publishRoomJoinRequestsUpdated(roomID)
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "room": detail, "invite": h.roomInvitePayload(inviteID, userID)})
		return
	}
	if h.isSuperuser(userID) {
		_, _ = h.DB.Exec(`UPDATE room_invites SET status = 'accepted', updated_at = ? WHERE room_id = ? AND target_user_id = ? AND status = 'pending'`, nowMillis(), roomID, userID)
		detail, err := h.buildRoomDetail(roomID, userID)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room")
			return
		}
		h.publishRoomInvitesUpdated(userID)
		c.JSON(http.StatusOK, gin.H{"ok": true, "room": detail, "invite": h.roomInvitePayload(inviteID, userID)})
		return
	}

	var joinPolicy string
	if err := h.DB.QueryRow(`SELECT join_policy FROM rooms WHERE id = ?`, roomID).Scan(&joinPolicy); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room")
		return
	}
	inviterIsAdmin := h.isAdmin(roomID, inviterID)
	if joinPolicy != "open" && !inviterIsAdmin && !hasPrivilegedInvite {
		tx, err := h.DB.Begin()
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "accept room invite failed")
			return
		}
		defer tx.Rollback()
		now := nowMillis()
		requestID := newID("jrq")
		reason := cleanJoinRequestReason(req.Reason)
		if _, err := tx.Exec(
			`INSERT INTO join_requests (id, room_id, user_id, status, reason, created_at, updated_at)
			 VALUES (?, ?, ?, 'pending', ?, ?, ?)
			 ON DUPLICATE KEY UPDATE
			   status = 'pending',
			   reason = VALUES(reason),
			   created_at = VALUES(created_at),
			   updated_at = VALUES(updated_at),
			   reviewer_user_id = NULL,
			   reviewed_at = NULL`,
			requestID, roomID, userID, reason, now, now,
		); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to create join request")
			return
		}
		_, _ = tx.Exec(`UPDATE room_invites SET status = 'accepted', updated_at = ? WHERE room_id = ? AND target_user_id = ? AND status = 'pending'`, now, roomID, userID)
		var status string
		var createdAt int64
		_ = tx.QueryRow(`SELECT id, status, created_at FROM join_requests WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&requestID, &status, &createdAt)
		if err := tx.Commit(); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "save room invite decision failed")
			return
		}
		h.publishRoomInvitesUpdated(userID)
		h.publishRoomApplicationsUpdated(userID)
		h.publishRoomJoinRequestsUpdated(roomID)
		c.JSON(http.StatusAccepted, gin.H{
			"ok": true,
			"join_request": gin.H{
				"id": requestID, "room_id": roomID, "status": status, "reason": reason, "created_at": formatMillis(createdAt),
			},
			"invite": h.roomInvitePayload(inviteID, userID),
		})
		return
	}

	tx, err := h.DB.Begin()
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "accept room invite failed")
		return
	}
	defer tx.Rollback()
	now := nowMillis()
	if _, err := h.addRoomMemberTx(tx, roomID, userID, now); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "accept room invite failed")
		return
	}
	if err := h.appendSystemMessageTx(tx, roomID, systemMessageSpec{
		Event:  systemEventRoomMemberJoined,
		UserID: userID,
	}); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "accept room invite failed")
		return
	}
	_, _ = tx.Exec(`UPDATE room_invites SET status = 'accepted', updated_at = ? WHERE room_id = ? AND target_user_id = ? AND status = 'pending'`, now, roomID, userID)
	var reviewerID any
	if inviterIsAdmin {
		reviewerID = inviterID
	}
	joinRequestUpdated := false
	if res, err := tx.Exec(
		`UPDATE join_requests
		 SET status = 'approved', updated_at = ?, reviewer_user_id = ?, reviewed_at = ?
		 WHERE room_id = ? AND user_id = ? AND status = 'pending'`,
		now, reviewerID, now, roomID, userID,
	); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "save room invite decision failed")
		return
	} else if n, _ := res.RowsAffected(); n > 0 {
		joinRequestUpdated = true
	}
	if err := tx.Commit(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "save room invite decision failed")
		return
	}

	detail, err := h.buildRoomDetail(roomID, userID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room")
		return
	}
	h.publishRoomToUser(userID, roomID, "room_added")
	h.publishRoomUpdated(roomID, userID)
	h.publishRoomInvitesUpdated(userID)
	h.publishRoomApplicationsUpdated(userID)
	if joinRequestUpdated {
		h.publishRoomJoinRequestsUpdated(roomID)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "room": detail, "invite": h.roomInvitePayload(inviteID, userID)})
}

func (h *Handler) removeMember(c *gin.Context) {
	roomID := c.Param("room_id")
	actorID := currentUserID(c)
	if c.Param("user_id") == "me" {
		h.leaveRoom(c)
		return
	}
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
	var role, targetNotificationLevel string
	_ = h.DB.QueryRow(
		`SELECT role, COALESCE(notification_level, 'all')
		 FROM room_memberships
		 WHERE room_id = ? AND user_id = ?`,
		roomID,
		targetID,
	).Scan(&role, &targetNotificationLevel)
	if role == "owner" {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot remove room owner")
		return
	}
	if role != "" && role != "member" && !h.canManageRoomRoles(roomID, actorID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot remove room admin")
		return
	}
	tx, err := h.DB.Begin()
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "remove member failed")
		return
	}
	defer tx.Rollback()
	liveRes, _ := tx.Exec(`DELETE FROM live_participants WHERE room_id = ? AND user_id = ?`, roomID, targetID)
	removedFromLive := false
	if n, _ := liveRes.RowsAffected(); n > 0 {
		removedFromLive = true
	}
	if err := h.deleteRoomInviteHistoryForTargetTx(tx, roomID, targetID); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "remove member failed")
		return
	}
	res, err := tx.Exec(`DELETE FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, targetID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "remove member failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.jsonError(c, http.StatusNotFound, "not_found", "member not found")
		return
	}
	pruned, err := h.pruneOrRepairRoomTx(tx, roomID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "repair room admins failed")
		return
	}
	if !pruned {
		messageID, err := h.appendSystemMessageTxWithID(tx, roomID, systemMessageSpec{
			Event:    systemEventRoomMemberRemoved,
			ActorID:  actorID,
			TargetID: targetID,
		})
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "remove member failed")
			return
		}
		if err := h.appendRoomNotificationTx(tx, roomNotificationSpec{
			Type:              roomNotificationMemberRemoved,
			RecipientID:       targetID,
			RoomID:            roomID,
			ActorID:           actorID,
			MessageID:         messageID,
			NotificationLevel: targetNotificationLevel,
		}); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "remove member failed")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "save membership failed")
		return
	}
	// Removed user drops the room; survivors get the bumped-down snapshot
	// (unless the room was pruned to empty, leaving no one to notify).
	h.publishRoomDeleted(roomID, targetID)
	h.publishPendingRoomInvitesUpdatedForInviter(roomID, targetID)
	h.publishRoomInvitesUpdated(targetID)
	if !pruned {
		h.publishRoomNotificationsUpdated(targetID)
	}
	if !pruned {
		h.publishRoomUpdated(roomID)
		if removedFromLive {
			h.PublishLiveSnapshot(roomID, "live_participant_left", map[string]any{"user_id": targetID})
		}
	} else {
		h.publishPendingRoomInvitesUpdatedForRoom(roomID)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) updateMemberRole(c *gin.Context) {
	roomID := c.Param("room_id")
	actorID := currentUserID(c)
	var req memberUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Role == nil && req.RoomDisplayName == nil {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "role or room_display_name is required")
		return
	}
	if req.Role != nil && !allowed(*req.Role, "admin", "member") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "role must be admin or member")
		return
	}
	if req.RoomDisplayName != nil && len([]rune(strings.TrimSpace(*req.RoomDisplayName))) > 32 {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "room_display_name must be 32 characters or fewer")
		return
	}
	targetID := c.Param("user_id")
	if h.isProtectedSuperuserTarget(actorID, targetID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot manage super user")
		return
	}
	var currentRole string
	if err := h.DB.QueryRow(`SELECT role FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, targetID).Scan(&currentRole); err != nil {
		h.jsonError(c, http.StatusNotFound, "not_found", "member not found")
		return
	}
	if req.Role != nil {
		if !h.canManageRoomRoles(roomID, actorID) {
			h.jsonError(c, http.StatusForbidden, "forbidden", "owner required")
			return
		}
		if currentRole == "owner" {
			h.jsonError(c, http.StatusForbidden, "forbidden", "cannot change owner role")
			return
		}
	}
	if req.RoomDisplayName != nil && !h.canEditMemberRoomDisplayName(roomID, actorID, targetID, currentRole) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot edit this member")
		return
	}
	tx, err := h.DB.Begin()
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "update member failed")
		return
	}
	defer tx.Rollback()
	roleChanged := false
	if req.Role != nil {
		res, err := tx.Exec(`UPDATE room_memberships SET role = ? WHERE room_id = ? AND user_id = ?`, *req.Role, roomID, targetID)
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
		if currentRole != *req.Role {
			roleChanged = true
			messageID, err := h.appendSystemMessageTxWithID(tx, roomID, systemMessageSpec{
				Event:    systemEventRoomRoleChanged,
				ActorID:  actorID,
				TargetID: targetID,
				FromRole: currentRole,
				ToRole:   *req.Role,
			})
			if err != nil {
				h.jsonError(c, http.StatusInternalServerError, "internal_error", "update role failed")
				return
			}
			if err := h.appendRoomNotificationTx(tx, roomNotificationSpec{
				Type:        roomRoleNotificationType(currentRole, *req.Role),
				RecipientID: targetID,
				RoomID:      roomID,
				ActorID:     actorID,
				FromRole:    currentRole,
				ToRole:      *req.Role,
				MessageID:   messageID,
			}); err != nil {
				h.jsonError(c, http.StatusInternalServerError, "internal_error", "update role failed")
				return
			}
		}
	}
	displayNameChanged := false
	if req.RoomDisplayName != nil {
		res, err := tx.Exec(
			`UPDATE room_memberships SET room_display_name = ? WHERE room_id = ? AND user_id = ?`,
			emptyToNil(*req.RoomDisplayName), roomID, targetID,
		)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "update member failed")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			h.jsonError(c, http.StatusNotFound, "not_found", "member not found")
			return
		}
		displayNameChanged = true
	}
	if err := tx.Commit(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "save membership failed")
		return
	}
	// Role is a personal field (my_role), not part of the shared snapshot, so a
	// room_updated wouldn't carry it. Tell the affected user directly so their
	// permissions UI reflects the change without a manual refetch.
	if req.Role != nil {
		h.publishRoomRole(roomID, targetID)
	}
	if roleChanged {
		h.publishRoomUpdated(roomID)
		h.publishRoomNotificationsUpdated(targetID)
	}
	if displayNameChanged {
		h.publishRoomMemberProfileChanged(roomID, targetID)
	}
	if displayNameChanged && !roleChanged {
		h.publishRoomUpdated(roomID)
	}
	c.JSON(http.StatusOK, gin.H{"member": h.memberPayload(roomID, targetID, actorID)})
}

func (h *Handler) canEditMemberRoomDisplayName(roomID, actorID, targetID, targetRole string) bool {
	if actorID == targetID {
		return false
	}
	actorRank := roleRank("")
	if h.isSuperuser(actorID) {
		if !h.roomIDExists(roomID) {
			return false
		}
		actorRank = 4
	} else {
		var actorRole string
		_ = h.DB.QueryRow(`SELECT role FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, actorID).Scan(&actorRole)
		actorRank = roleRank(actorRole)
	}
	targetRank := roleRank(targetRole)
	if h.isSuperuser(targetID) {
		targetRank = 4
	}
	return actorRank > targetRank
}

func (h *Handler) transferRoomCreator(c *gin.Context) {
	roomID := c.Param("room_id")
	actorID := currentUserID(c)
	if !h.canManageRoomRoles(roomID, actorID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "owner required")
		return
	}
	var req userIDRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.UserID) == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "user_id is required")
		return
	}
	targetID := strings.TrimSpace(req.UserID)
	if h.isSuperuser(targetID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "super user cannot become room creator")
		return
	}
	var targetRole string
	if err := h.DB.QueryRow(`SELECT role FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, targetID).Scan(&targetRole); err != nil {
		h.jsonError(c, http.StatusNotFound, "not_found", "member not found")
		return
	}
	rows, err := h.DB.Query(`SELECT user_id FROM room_memberships WHERE room_id = ? AND role = 'owner'`, roomID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "transfer creator failed")
		return
	}
	previousOwnerIDs := make([]string, 0, 1)
	for rows.Next() {
		var ownerID string
		if err := rows.Scan(&ownerID); err != nil {
			rows.Close()
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "transfer creator failed")
			return
		}
		previousOwnerIDs = append(previousOwnerIDs, ownerID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "transfer creator failed")
		return
	}
	if err := rows.Close(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "transfer creator failed")
		return
	}
	tx, err := h.DB.Begin()
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "transfer creator failed")
		return
	}
	defer tx.Rollback()
	roomNotificationRecipients := map[string]bool{}
	_, _ = tx.Exec(`UPDATE room_memberships SET role = 'admin' WHERE room_id = ? AND role = 'owner' AND user_id != ?`, roomID, targetID)
	if _, err := tx.Exec(`UPDATE room_memberships SET role = 'owner' WHERE room_id = ? AND user_id = ?`, roomID, targetID); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "transfer creator failed")
		return
	}
	if _, err := tx.Exec(`UPDATE rooms SET created_by_user_id = ?, updated_at = ? WHERE id = ?`, targetID, nowMillis(), roomID); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "transfer creator failed")
		return
	}
	if targetRole != "owner" {
		messageID, err := h.appendSystemMessageTxWithID(tx, roomID, systemMessageSpec{
			Event:    systemEventRoomRoleChanged,
			ActorID:  actorID,
			TargetID: targetID,
			FromRole: targetRole,
			ToRole:   "owner",
		})
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "transfer creator failed")
			return
		}
		if err := h.appendRoomNotificationTx(tx, roomNotificationSpec{
			Type:        roomNotificationRolePromoted,
			RecipientID: targetID,
			RoomID:      roomID,
			ActorID:     actorID,
			FromRole:    targetRole,
			ToRole:      "owner",
			MessageID:   messageID,
		}); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "transfer creator failed")
			return
		}
		roomNotificationRecipients[targetID] = true
	}
	for _, ownerID := range previousOwnerIDs {
		if ownerID == targetID {
			continue
		}
		messageID, err := h.appendSystemMessageTxWithID(tx, roomID, systemMessageSpec{
			Event:    systemEventRoomRoleChanged,
			ActorID:  actorID,
			TargetID: ownerID,
			FromRole: "owner",
			ToRole:   "admin",
		})
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "transfer creator failed")
			return
		}
		if err := h.appendRoomNotificationTx(tx, roomNotificationSpec{
			Type:        roomNotificationCreatorTransferDemoted,
			RecipientID: ownerID,
			RoomID:      roomID,
			FromRole:    "owner",
			ToRole:      "admin",
			MessageID:   messageID,
		}); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "transfer creator failed")
			return
		}
		roomNotificationRecipients[ownerID] = true
	}
	if err := tx.Commit(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "save creator transfer failed")
		return
	}
	notified := map[string]bool{}
	publishRoleOnce := func(userID string) {
		if userID == "" || notified[userID] {
			return
		}
		notified[userID] = true
		h.publishRoomRole(roomID, userID)
	}
	publishRoleOnce(targetID)
	for _, ownerID := range previousOwnerIDs {
		publishRoleOnce(ownerID)
	}
	for userID := range roomNotificationRecipients {
		h.publishRoomNotificationsUpdated(userID)
	}
	h.publishRoomUpdated(roomID)
	detail, err := h.buildRoomDetail(roomID, actorID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room")
		return
	}
	c.JSON(http.StatusOK, gin.H{"room": detail})
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
		`SELECT jr.id, jr.status, jr.reason, jr.created_at, u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key
		 FROM join_requests jr JOIN users u ON u.id = jr.user_id
		 WHERE jr.room_id = ? AND jr.status = ?
		   AND NOT EXISTS (
		     SELECT 1
		     FROM room_blacklist rb
		     WHERE rb.room_id = jr.room_id
		       AND rb.user_id = jr.user_id
		   )
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
		request, targetUserID, requestCreatedAt, err := scanJoinRequest(rows)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "read join request failed")
			return
		}
		source, inviters := h.joinRequestSourcePayload(roomID, targetUserID, requestCreatedAt)
		request["source"] = source
		request["inviters"] = inviters
		requests = append(requests, request)
	}
	c.JSON(http.StatusOK, gin.H{"requests": requests, "next_cursor": nil})
}

func (h *Handler) reviewJoinRequest(c *gin.Context) {
	roomID := c.Param("room_id")
	reviewerID := currentUserID(c)
	if !h.isAdmin(roomID, reviewerID) {
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
		if h.isRoomBlacklisted(roomID, userID) {
			h.jsonError(c, http.StatusConflict, "blocked", "user is blocked from this room")
			return
		}
		if !h.isSuperuser(userID) {
			var alreadyMember int
			_ = h.DB.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&alreadyMember)
			tx, err := h.DB.Begin()
			if err != nil {
				h.jsonError(c, http.StatusInternalServerError, "internal_error", "approve join request failed")
				return
			}
			defer tx.Rollback()
			now := nowMillis()
			if _, err := h.addRoomMemberTx(tx, roomID, userID, now); err != nil {
				h.jsonError(c, http.StatusInternalServerError, "internal_error", "approve join request failed")
				return
			}
			if alreadyMember == 0 {
				if err := h.appendSystemMessageTx(tx, roomID, systemMessageSpec{
					Event:  systemEventRoomMemberJoined,
					UserID: userID,
				}); err != nil {
					h.jsonError(c, http.StatusInternalServerError, "internal_error", "approve join request failed")
					return
				}
			}
			if _, err := tx.Exec(
				`UPDATE join_requests
				 SET status = ?, updated_at = ?, reviewer_user_id = ?, reviewed_at = ?
				 WHERE id = ?`,
				newStatus, now, reviewerID, now, c.Param("request_id"),
			); err != nil {
				h.jsonError(c, http.StatusInternalServerError, "internal_error", "approve join request failed")
				return
			}
			if err := tx.Commit(); err != nil {
				h.jsonError(c, http.StatusInternalServerError, "internal_error", "approve join request failed")
				return
			}
		} else {
			now := nowMillis()
			_, _ = h.DB.Exec(
				`UPDATE join_requests
				 SET status = ?, updated_at = ?, reviewer_user_id = ?, reviewed_at = ?
				 WHERE id = ?`,
				newStatus, now, reviewerID, now, c.Param("request_id"),
			)
		}
	} else {
		now := nowMillis()
		_, _ = h.DB.Exec(
			`UPDATE join_requests
			 SET status = ?, updated_at = ?, reviewer_user_id = ?, reviewed_at = ?
			 WHERE id = ?`,
			newStatus, now, reviewerID, now, c.Param("request_id"),
		)
	}
	if newStatus == "approved" {
		// The applicant's existing SSE connection isn't subscribed to this room
		// (they weren't a member when it connected), so room_added reaches them
		// by userID. Existing members get the bumped member_count.
		if !h.isSuperuser(userID) {
			h.publishRoomToUser(userID, roomID, "room_added")
		}
		h.publishRoomUpdated(roomID, userID)
	}
	h.publishRoomApplicationsUpdated(userID)
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
		ConfirmRID  string `json:"confirm_rid"`
		ConfirmName string `json:"confirm_name"`
	}
	_ = c.ShouldBindJSON(&req)
	var rid, name string
	_ = h.DB.QueryRow(`SELECT rid, name FROM rooms WHERE id = ?`, roomID).Scan(&rid, &name)
	if req.ConfirmRID == "" && req.ConfirmName == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "confirm_name or confirm_rid is required")
		return
	}
	if req.ConfirmRID != "" && req.ConfirmRID != rid {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "confirm_rid mismatch")
		return
	}
	if req.ConfirmName != "" && req.ConfirmName != name {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "confirm_name mismatch")
		return
	}
	// Capture the audience before the row is gone — afterwards there's no
	// membership left to enumerate.
	members, _ := h.roomMemberIDs(roomID)
	pendingInviteTargets := h.pendingRoomInviteTargetIDs(roomID)
	_, _ = h.DB.Exec(`DELETE FROM rooms WHERE id = ?`, roomID)
	h.publishRoomDeleted(roomID, members...)
	h.publishRoomInvitesUpdatedForUsers(pendingInviteTargets...)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) roomSettingsPayload(roomID string) gin.H {
	var rid, name, defaultAvatar, visibility, joinPolicy, recallPolicy, description string
	var avatarAssetID, avatarURL sql.NullString
	var ai int
	var recallWindow sql.NullInt64
	var createdAt, updatedAt int64
	_ = h.DB.QueryRow(
		`SELECT rid, name, avatar_asset_id, avatar_url, default_avatar_key, visibility, join_policy,
		        ai_voice_announce_enabled, message_recall_policy, message_recall_window_seconds,
		        description, created_at, updated_at
		 FROM rooms WHERE id = ?`,
		roomID,
	).Scan(&rid, &name, &avatarAssetID, &avatarURL, &defaultAvatar, &visibility, &joinPolicy, &ai, &recallPolicy, &recallWindow, &description, &createdAt, &updatedAt)
	return gin.H{
		"id": roomID, "rid": rid, "name": name, "avatar_asset_id": nullableString(avatarAssetID),
		"avatar_url": nullableString(avatarURL), "default_avatar_key": defaultAvatar,
		"description": description,
		"visibility":  visibility, "join_policy": joinPolicy, "ai_voice_announce_enabled": ai != 0,
		"ai_voice_announcements_enabled": ai != 0,
		"message_recall_policy":          recallPolicy, "message_recall_window_seconds": nullableInt64(recallWindow),
		"created_at": formatMillis(createdAt), "updated_at": formatMillis(updatedAt),
	}
}

func (h *Handler) myRoomSettingsPayload(roomID, userID string) gin.H {
	var remark, display sql.NullString
	var notification string
	var isPinned int
	_ = h.DB.QueryRow(
		`SELECT remark_name, room_display_name, notification_level, is_pinned
		 FROM room_memberships WHERE room_id = ? AND user_id = ?`,
		roomID, userID,
	).Scan(&remark, &display, &notification, &isPinned)
	return gin.H{
		"remark_name": nullableString(remark), "room_display_name": nullableString(display),
		"notification_level":  notification,
		"notification_policy": notification, "is_pinned": isPinned != 0,
	}
}

func (h *Handler) memberPayload(roomID, userID, viewerID string) gin.H {
	var id, uid, username, role string
	var displayName, avatarURL, defaultAvatar, roomDisplayName sql.NullString
	var joinedAt int64
	_ = h.DB.QueryRow(
		`SELECT u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key, rm.role, rm.joined_at, rm.room_display_name
		 FROM room_memberships rm JOIN users u ON u.id = rm.user_id
		 WHERE rm.room_id = ? AND rm.user_id = ?`,
		roomID, userID,
	).Scan(&id, &uid, &username, &displayName, &avatarURL, &defaultAvatar, &role, &joinedAt, &roomDisplayName)
	user := summaryFromUserFields(id, uid, username, displayName, avatarURL, defaultAvatar)
	user.RoomDisplayName = nullableString(roomDisplayName)
	isOnline := h.isUserOnlineForViewer(id, viewerID)
	user.IsOnline = &isOnline
	return gin.H{
		"user":              user,
		"role":              role,
		"room_display_name": nullableString(roomDisplayName),
		"is_online":         isOnline,
		"joined_at":         formatMillis(joinedAt),
	}
}

func (h *Handler) deleteRoomInviteHistoryForTargetTx(tx *sql.Tx, roomID, targetUserID string) error {
	_, err := tx.Exec(`DELETE FROM room_invites WHERE room_id = ? AND target_user_id = ?`, roomID, targetUserID)
	return err
}

type blacklistEntryScanner interface {
	Scan(dest ...any) error
}

func (h *Handler) roomBlacklistEntryPayload(roomID, userID string) (gin.H, error) {
	return scanRoomBlacklistEntry(h.DB.QueryRow(
		`SELECT rb.user_id, rb.blocked_by_user_id, rb.created_at,
		        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
		        bu.id, bu.uid, bu.username, bu.display_name, bu.avatar_url, bu.default_avatar_key
		 FROM room_blacklist rb
		 JOIN users u ON u.id = rb.user_id
		 LEFT JOIN users bu ON bu.id = rb.blocked_by_user_id
		 WHERE rb.room_id = ? AND rb.user_id = ?`,
		roomID, userID,
	))
}

func scanRoomBlacklistEntry(scanner blacklistEntryScanner) (gin.H, error) {
	var userID string
	var blockedByUserID, blockedByID, blockedByUID, blockedByUsername sql.NullString
	var id, uid, username string
	var displayName, avatarURL, defaultAvatar, blockedByDisplayName, blockedByAvatarURL, blockedByDefaultAvatar sql.NullString
	var createdAt int64
	if err := scanner.Scan(
		&userID, &blockedByUserID, &createdAt,
		&id, &uid, &username, &displayName, &avatarURL, &defaultAvatar,
		&blockedByID, &blockedByUID, &blockedByUsername, &blockedByDisplayName, &blockedByAvatarURL, &blockedByDefaultAvatar,
	); err != nil {
		return nil, err
	}
	entry := gin.H{
		"user":       summaryFromUserFields(id, uid, username, displayName, avatarURL, defaultAvatar),
		"created_at": formatMillis(createdAt),
	}
	if blockedByID.Valid && blockedByUsername.Valid {
		entry["blocked_by"] = summaryFromUserFields(
			blockedByID.String,
			blockedByUID.String,
			blockedByUsername.String,
			blockedByDisplayName,
			blockedByAvatarURL,
			blockedByDefaultAvatar,
		)
	} else {
		entry["blocked_by"] = nil
	}
	_ = userID
	_ = blockedByUserID
	return entry, nil
}

func scanJoinRequest(rows *sql.Rows) (gin.H, string, int64, error) {
	var requestID, status, reason, id, uid, username string
	var displayName, avatarURL, defaultAvatar sql.NullString
	var createdAt int64
	if err := rows.Scan(&requestID, &status, &reason, &createdAt, &id, &uid, &username, &displayName, &avatarURL, &defaultAvatar); err != nil {
		return nil, "", 0, err
	}
	return gin.H{
		"id": requestID, "status": status, "reason": reason, "created_at": formatMillis(createdAt),
		"user": summaryFromUserFields(id, uid, username, displayName, avatarURL, defaultAvatar),
	}, id, createdAt, nil
}

func (h *Handler) joinRequestSourcePayload(roomID, targetUserID string, requestCreatedAt int64) (string, []userSummary) {
	rows, err := h.DB.Query(
		`SELECT DISTINCT u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
		        irm.room_display_name, irm.role
		 FROM room_invites ri
		 JOIN users u ON u.id = ri.inviter_user_id
		 LEFT JOIN room_memberships irm ON irm.room_id = ri.room_id AND irm.user_id = ri.inviter_user_id
		 WHERE ri.room_id = ? AND ri.target_user_id = ? AND ri.status IN ('pending', 'accepted') AND ri.updated_at >= ?
		 ORDER BY ri.created_at ASC`,
		roomID, targetUserID, requestCreatedAt,
	)
	if err != nil {
		return "public_search", []userSummary{}
	}
	defer rows.Close()

	inviters := make([]userSummary, 0)
	for rows.Next() {
		var id, uid, username string
		var displayName, avatarURL, defaultAvatar, roomDisplayName, roomRole sql.NullString
		if err := rows.Scan(&id, &uid, &username, &displayName, &avatarURL, &defaultAvatar, &roomDisplayName, &roomRole); err != nil {
			continue
		}
		summary := summaryFromUserFields(id, uid, username, displayName, avatarURL, defaultAvatar)
		summary.RoomDisplayName = nullableString(roomDisplayName)
		if roomRole.Valid && roomRole.String != "" {
			summary.RoomRole = roomRole.String
		} else if h.isSuperuser(id) {
			summary.RoomRole = "superuser"
		} else {
			summary.RoomRole = "left"
		}
		inviters = append(inviters, summary)
	}
	if len(inviters) == 0 {
		return "public_search", inviters
	}
	return "invitation", inviters
}

func (h *Handler) roomInvitePayload(inviteID, viewerID string) gin.H {
	var id, roomID, status, inviterID string
	var inviterUserID, inviterUID, inviterUsername sql.NullString
	var inviterDisplayName, inviterAvatarURL, inviterDefaultAvatar, inviterRoomDisplayName, inviterRoomRole sql.NullString
	var rid, name, defaultAvatar, visibility, joinPolicy string
	var avatarURL, roomDescription, roomCreatedByUserID sql.NullString
	var createdAt, updatedAt int64
	var roomExists int
	err := h.DB.QueryRow(
		`SELECT ri.id, ri.room_id, ri.status, ri.created_at, ri.updated_at,
		        ri.inviter_user_id,
		        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
		        irm.room_display_name, irm.role,
		        COALESCE(r.rid, ri.room_rid),
		        COALESCE(r.name, ri.room_name),
		        COALESCE(r.avatar_url, ri.room_avatar_url),
		        COALESCE(r.default_avatar_key, ri.room_default_avatar_key),
		        COALESCE(r.visibility, ri.room_visibility),
		        COALESCE(r.join_policy, ri.room_join_policy),
		        r.description,
		        r.created_by_user_id,
		        CASE WHEN r.id IS NULL THEN 0 ELSE 1 END
		 FROM room_invites ri
		 LEFT JOIN users u ON u.id = ri.inviter_user_id
		 LEFT JOIN rooms r ON r.id = ri.room_id
		 LEFT JOIN room_memberships irm ON irm.room_id = ri.room_id AND irm.user_id = ri.inviter_user_id
		 WHERE ri.id = ?`,
		inviteID,
	).Scan(
		&id, &roomID, &status, &createdAt, &updatedAt,
		&inviterID,
		&inviterUserID, &inviterUID, &inviterUsername, &inviterDisplayName, &inviterAvatarURL, &inviterDefaultAvatar,
		&inviterRoomDisplayName, &inviterRoomRole,
		&rid, &name, &avatarURL, &defaultAvatar, &visibility, &joinPolicy, &roomDescription, &roomCreatedByUserID, &roomExists,
	)
	if err != nil {
		return gin.H{"id": inviteID}
	}
	inviterExists := inviterUserID.Valid && inviterUsername.Valid
	inviterSummaryID := inviterID
	inviterSummaryUID := ""
	inviterSummaryUsername := ""
	if inviterExists {
		inviterSummaryID = inviterUserID.String
		inviterSummaryUID = inviterUID.String
		inviterSummaryUsername = inviterUsername.String
	} else {
		inviterDisplayName = sql.NullString{String: "用户不存在", Valid: true}
		inviterAvatarURL = sql.NullString{}
		inviterDefaultAvatar = sql.NullString{String: "graphite-2", Valid: true}
	}
	inviter := summaryFromUserFields(
		inviterSummaryID,
		inviterSummaryUID,
		inviterSummaryUsername,
		inviterDisplayName,
		inviterAvatarURL,
		inviterDefaultAvatar,
	)
	inviter.RoomDisplayName = nullableString(inviterRoomDisplayName)
	inviter.IsSuperuser = h.isSuperuser(inviterID)
	if inviterRoomRole.Valid && inviterRoomRole.String != "" {
		inviter.RoomRole = inviterRoomRole.String
	} else if inviter.IsSuperuser {
		inviter.RoomRole = "superuser"
	} else {
		inviter.RoomRole = "left"
	}
	invalidReason := ""
	if status == "pending" {
		invalidReason = h.roomInviteInvalidReason(roomID, inviterID, viewerID)
	}
	return gin.H{
		"id": id, "status": status, "created_at": formatMillis(createdAt), "updated_at": formatMillis(updatedAt),
		"room_exists": roomExists != 0, "invalid_reason": nullableStringFromText(invalidReason),
		"inviter_exists": inviterExists,
		"room": h.roomNotificationRoomPayload(
			roomID, viewerID, rid, name, defaultAvatar, visibility, joinPolicy,
			avatarURL, roomDescription, roomCreatedByUserID, roomExists != 0,
		),
		"inviter": inviter,
	}
}

func (h *Handler) roomInviteInvalidReason(roomID, inviterID, targetUserID string) string {
	if !h.roomIDExists(roomID) {
		return "room_missing"
	}
	if targetUserID != "" && h.isRoomBlacklisted(roomID, targetUserID) {
		return "target_blocked"
	}
	if h.isRoomJoinPolicyClosed(roomID) {
		return "room_closed"
	}
	if h.isSuperuser(inviterID) {
		return ""
	}
	var membershipCount int
	_ = h.DB.QueryRow(
		`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`,
		roomID, inviterID,
	).Scan(&membershipCount)
	if membershipCount == 0 {
		return "inviter_left"
	}
	return ""
}

func (h *Handler) hasPrivilegedPendingRoomInvite(roomID, targetUserID string) bool {
	if h.isRoomBlacklisted(roomID, targetUserID) {
		return false
	}
	if h.isRoomJoinPolicyClosed(roomID) {
		return false
	}
	rows, err := h.DB.Query(
		`SELECT inviter_user_id
		 FROM room_invites
		 WHERE room_id = ? AND target_user_id = ? AND status = 'pending'`,
		roomID, targetUserID,
	)
	if err != nil {
		return false
	}
	defer rows.Close()

	for rows.Next() {
		var inviterID string
		if err := rows.Scan(&inviterID); err != nil {
			continue
		}
		if h.isPrivilegedRoomInviter(roomID, inviterID) {
			return true
		}
	}
	return false
}

func (h *Handler) isRoomJoinPolicyClosed(roomID string) bool {
	var joinPolicy string
	if err := h.DB.QueryRow(`SELECT join_policy FROM rooms WHERE id = ?`, roomID).Scan(&joinPolicy); err != nil {
		return false
	}
	return roomJoinPolicyClosed(joinPolicy)
}

func roomJoinPolicyClosed(joinPolicy string) bool {
	return strings.EqualFold(strings.TrimSpace(joinPolicy), "closed")
}

func (h *Handler) isPrivilegedRoomInviter(roomID, userID string) bool {
	if h.isSuperuser(userID) {
		return h.roomIDExists(roomID)
	}
	var role string
	_ = h.DB.QueryRow(
		`SELECT role FROM room_memberships WHERE room_id = ? AND user_id = ?`,
		roomID, userID,
	).Scan(&role)
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "owner", "creator", "admin", "administrator", "superuser":
		return true
	default:
		return false
	}
}

func (h *Handler) roomApplicationPayload(requestID, viewerID string) gin.H {
	var id, roomID, status, reason string
	var createdAt, updatedAt int64
	var reviewerRefID, reviewerUserID, reviewerUID, reviewerUsername sql.NullString
	var reviewerDisplayName, reviewerAvatarURL, reviewerDefaultAvatar, reviewerRoomDisplayName, reviewerRoomRole sql.NullString
	var reviewedAt sql.NullInt64
	var rid, name, defaultAvatar, visibility, joinPolicy string
	var avatarURL, roomDescription, roomCreatedByUserID sql.NullString
	err := h.DB.QueryRow(
		`SELECT jr.id, jr.room_id, jr.status, jr.reason, jr.created_at, jr.updated_at,
		        jr.reviewer_user_id, jr.reviewed_at,
		        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
		        rrm.room_display_name, rrm.role,
		        r.rid, r.name, r.avatar_url, r.default_avatar_key, r.visibility, r.join_policy,
		        r.description, r.created_by_user_id
		 FROM join_requests jr
		 JOIN rooms r ON r.id = jr.room_id
		 LEFT JOIN users u ON u.id = jr.reviewer_user_id
		 LEFT JOIN room_memberships rrm ON rrm.room_id = jr.room_id AND rrm.user_id = jr.reviewer_user_id
		 WHERE jr.id = ? AND jr.user_id = ?`,
		requestID, viewerID,
	).Scan(
		&id, &roomID, &status, &reason, &createdAt, &updatedAt,
		&reviewerRefID, &reviewedAt,
		&reviewerUserID, &reviewerUID, &reviewerUsername, &reviewerDisplayName, &reviewerAvatarURL, &reviewerDefaultAvatar,
		&reviewerRoomDisplayName, &reviewerRoomRole,
		&rid, &name, &avatarURL, &defaultAvatar, &visibility, &joinPolicy, &roomDescription, &roomCreatedByUserID,
	)
	if err != nil {
		return gin.H{"id": requestID}
	}

	var reviewer any
	reviewerExists := true
	if reviewerRefID.Valid && reviewerRefID.String != "" {
		reviewerExists = reviewerUserID.Valid && reviewerUsername.Valid
		reviewerSummaryID := reviewerRefID.String
		reviewerSummaryUID := ""
		reviewerSummaryUsername := ""
		if reviewerExists {
			reviewerSummaryID = reviewerUserID.String
			reviewerSummaryUID = reviewerUID.String
			reviewerSummaryUsername = reviewerUsername.String
		} else {
			reviewerDisplayName = sql.NullString{String: "用户不存在", Valid: true}
			reviewerAvatarURL = sql.NullString{}
			reviewerDefaultAvatar = sql.NullString{String: "graphite-2", Valid: true}
		}
		summary := summaryFromUserFields(
			reviewerSummaryID,
			reviewerSummaryUID,
			reviewerSummaryUsername,
			reviewerDisplayName,
			reviewerAvatarURL,
			reviewerDefaultAvatar,
		)
		summary.RoomDisplayName = nullableString(reviewerRoomDisplayName)
		if reviewerRoomRole.Valid && reviewerRoomRole.String != "" {
			summary.RoomRole = reviewerRoomRole.String
		} else if h.isSuperuser(reviewerRefID.String) {
			summary.RoomRole = "superuser"
		}
		reviewer = summary
	}

	return gin.H{
		"id": id, "status": status, "reason": reason, "created_at": formatMillis(createdAt), "updated_at": formatMillis(updatedAt),
		"reviewed_at":     nullableMillis(reviewedAt),
		"reviewer_exists": reviewerExists,
		"room": h.roomNotificationRoomPayload(
			roomID, viewerID, rid, name, defaultAvatar, visibility, joinPolicy,
			avatarURL, roomDescription, roomCreatedByUserID, true,
		),
		"reviewer": reviewer,
	}
}

func (h *Handler) roomNotificationRoomPayload(
	roomID, viewerID, rid, name, defaultAvatar, visibility, joinPolicy string,
	avatarURL, description, createdByUserID sql.NullString,
	roomExists bool,
) gin.H {
	memberCount := 0
	onlineCount := 0
	liveCount := 0
	joined := false
	if roomExists {
		memberCount, _ = h.memberCount(roomID)
		onlineCount, _ = h.onlineMemberCount(roomID)
		_, liveCount, _ = h.livePreview(roomID)
		var joinedCount int
		_ = h.DB.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, viewerID).Scan(&joinedCount)
		joined = joinedCount > 0
	}

	room := gin.H{
		"id": roomID, "rid": rid, "name": name,
		"description": description.String,
		"avatar_url":  nullableString(avatarURL), "default_avatar_key": defaultAvatar,
		"visibility": visibility, "join_policy": joinPolicy,
		"member_count": memberCount, "online_member_count": onlineCount, "live_participant_count": liveCount,
		"joined": joined, "join_state": h.joinState(roomID, viewerID, joined),
	}

	if !roomExists {
		return room
	}
	if createdByUserID.Valid && createdByUserID.String != "" {
		rec := roomRecord{ID: roomID, CreatedByUserID: createdByUserID}
		if !h.shouldHideRoomCreator(rec) {
			if summary, err := h.userSummary(createdByUserID.String); err == nil {
				room["created_by"] = summary
			}
		}
	}
	if profile, membership, ok := h.roomViewerMembershipPayload(roomID, viewerID); ok {
		room["personal_profile"] = profile
		room["my_membership"] = membership
	}
	return room
}

func (h *Handler) roomViewerMembershipPayload(roomID, userID string) (roomPersonalProfile, roomMembership, bool) {
	var role, notificationLevel string
	var remarkName, roomDisplayName sql.NullString
	var joinedAt int64
	var isPinned int
	err := h.DB.QueryRow(
		`SELECT role, notification_level, remark_name, room_display_name,
		        is_pinned, joined_at
		 FROM room_memberships
		 WHERE room_id = ? AND user_id = ?`,
		roomID, userID,
	).Scan(&role, &notificationLevel, &remarkName, &roomDisplayName, &isPinned, &joinedAt)
	if err != nil {
		return roomPersonalProfile{}, roomMembership{}, false
	}

	return roomPersonalProfile{
			DisplayName: nullableString(roomDisplayName),
		}, roomMembership{
			Role:              role,
			JoinedAt:          formatMillis(joinedAt),
			RemarkName:        nullableString(remarkName),
			RoomDisplayName:   nullableString(roomDisplayName),
			NotificationLevel: notificationLevel,
			IsPinned:          isPinned != 0,
		}, true
}

func cleanJoinRequestReason(value string) string {
	return strings.TrimSpace(value)
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

func (h *Handler) currentRoomAvatarKey(roomID string) string {
	var key string
	_ = h.DB.QueryRow(`SELECT default_avatar_key FROM rooms WHERE id = ?`, roomID).Scan(&key)
	return key
}
