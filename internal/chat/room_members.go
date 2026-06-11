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
	var roomDisplayName, roomAvatarURL, roomDefaultAvatar sql.NullString
	var textMutedUntil sql.NullInt64
	var joinedAt int64
	err := h.DB.QueryRow(
		`SELECT u.id,
		        rm.room_display_name, rm.room_avatar_url, rm.room_default_avatar_key,
		        rm.role, rm.text_muted_until, rm.joined_at
		 FROM room_memberships rm JOIN users u ON u.id = rm.user_id
		 WHERE rm.room_id = ? AND rm.user_id = ?`,
		roomID, targetID,
	).Scan(&id, &roomDisplayName, &roomAvatarURL, &roomDefaultAvatar, &role, &textMutedUntil, &joinedAt)
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
			"user":                    user,
			"room_display_name":       nil,
			"room_avatar_url":         nil,
			"room_default_avatar_key": nil,
			"role":                    "superuser",
			"text_muted_until":        nil,
			"joined_at":               formatMillis(nowMillis()),
		}})
		return
	}
	user, err := h.profileUserSummary(id, viewerID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read member profile")
		return
	}
	c.JSON(http.StatusOK, gin.H{"profile": gin.H{
		"user":                    user,
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
		if !allowed(*notificationPolicy, "all", "mentions", "muted") {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid "+notificationField)
			return
		}
		sets = append(sets, "notification_level = ?")
		args = append(args, *notificationPolicy)
	}
	avatarAssetID := req.RoomAvatarAssetID
	if avatarAssetID == nil {
		avatarAssetID = req.AvatarAssetID
	}
	if avatarAssetID != nil {
		var url sql.NullString
		assetID := strings.TrimSpace(*avatarAssetID)
		if assetID != "" {
			_ = h.DB.QueryRow(`SELECT url FROM assets WHERE id = ? AND owner_user_id = ?`, assetID, userID).Scan(&url)
		}
		sets = append(sets, "room_avatar_url = ?", "room_default_avatar_key = ?")
		if url.Valid {
			args = append(args, url.String, nil)
		} else {
			args = append(args, nil, nil)
		}
	}
	if req.DefaultAvatarKey != nil {
		key := strings.TrimSpace(*req.DefaultAvatarKey)
		sets = append(sets, "room_default_avatar_key = ?")
		args = append(args, emptyToNil(key))
		if avatarAssetID == nil && key != "" && h.currentRoomProfileAvatarKey(roomID, userID) != key {
			sets = append(sets, "room_avatar_url = ?")
			args = append(args, nil)
		}
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
	if req.Description != nil {
		description := strings.TrimSpace(*req.Description)
		if len([]rune(description)) > 500 {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "description must be at most 500 characters")
			return
		}
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
			var url string
			if err := h.DB.QueryRow(`SELECT url FROM assets WHERE id = ? AND owner_user_id = ?`, assetID, currentUserID(c)).Scan(&url); err != nil {
				h.jsonError(c, http.StatusBadRequest, "validation_failed", "avatar asset not found")
				return
			}
			sets = append(sets, "avatar_asset_id = ?", "avatar_url = ?")
			args = append(args, assetID, url)
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
	if _, err := h.DB.Exec(`UPDATE rooms SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "update room settings failed")
		return
	}
	// Name / avatar / visibility / join_policy etc. all live in the room-list
	// snapshot, so every member's list entry needs to be refreshed.
	h.publishRoomUpdated(roomID)
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
		 ON CONFLICT(room_id, target_user_id, inviter_user_id) DO UPDATE SET
		   status = 'pending',
		   created_at = excluded.created_at,
		   updated_at = excluded.updated_at,
		   room_rid = excluded.room_rid,
		   room_name = excluded.room_name,
		   room_avatar_url = excluded.room_avatar_url,
		   room_default_avatar_key = excluded.room_default_avatar_key,
		   room_visibility = excluded.room_visibility,
		   room_join_policy = excluded.room_join_policy`,
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
	if reason := h.roomInviteInvalidReason(roomID, inviterID); reason != "" {
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
			 ON CONFLICT(room_id, user_id) DO UPDATE SET
			   status = 'pending',
			   reason = excluded.reason,
			   created_at = excluded.created_at,
			   updated_at = excluded.updated_at,
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
	_, _ = tx.Exec(`UPDATE room_invites SET status = 'accepted', updated_at = ? WHERE room_id = ? AND target_user_id = ? AND status = 'pending'`, now, roomID, userID)
	var reviewerID any
	if inviterIsAdmin {
		reviewerID = inviterID
	}
	_, _ = tx.Exec(
		`UPDATE join_requests
		 SET status = 'approved', updated_at = ?, reviewer_user_id = ?, reviewed_at = ?
		 WHERE room_id = ? AND user_id = ? AND status = 'pending'`,
		now, reviewerID, now, roomID, userID,
	)
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
	liveRes, _ := tx.Exec(`DELETE FROM live_participants WHERE room_id = ? AND user_id = ?`, roomID, targetID)
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
		h.publishRoomUpdated(roomID)
		if n, _ := liveRes.RowsAffected(); n > 0 {
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
	if !h.canManageRoomRoles(roomID, actorID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "owner required")
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
	// Role is a personal field (my_role), not part of the shared snapshot, so a
	// room_updated wouldn't carry it. Tell the affected user directly so their
	// permissions UI reflects the change without a manual refetch.
	h.publishRoomRole(roomID, targetID)
	c.JSON(http.StatusOK, gin.H{"member": h.memberPayload(roomID, targetID)})
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
	_, _ = tx.Exec(`UPDATE room_memberships SET role = 'admin' WHERE room_id = ? AND role = 'owner' AND user_id != ?`, roomID, targetID)
	if _, err := tx.Exec(`UPDATE room_memberships SET role = 'owner' WHERE room_id = ? AND user_id = ?`, roomID, targetID); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "transfer creator failed")
		return
	}
	if _, err := tx.Exec(`UPDATE rooms SET created_by_user_id = ?, updated_at = ? WHERE id = ?`, targetID, nowMillis(), roomID); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "transfer creator failed")
		return
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
		if !h.isSuperuser(userID) {
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
		"notification_policy": notification,
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

func (h *Handler) deleteRoomInviteHistoryForTargetTx(tx *sql.Tx, roomID, targetUserID string) error {
	_, err := tx.Exec(`DELETE FROM room_invites WHERE room_id = ? AND target_user_id = ?`, roomID, targetUserID)
	return err
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
	var id, roomID, status, inviterID, inviterUID, inviterUsername string
	var inviterDisplayName, inviterAvatarURL, inviterDefaultAvatar, inviterRoomDisplayName, inviterRoomRole sql.NullString
	var rid, name, defaultAvatar, visibility, joinPolicy string
	var avatarURL sql.NullString
	var createdAt, updatedAt int64
	var roomExists int
	err := h.DB.QueryRow(
		`SELECT ri.id, ri.room_id, ri.status, ri.created_at, ri.updated_at,
		        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
		        irm.room_display_name, irm.role,
		        COALESCE(r.rid, ri.room_rid),
		        COALESCE(r.name, ri.room_name),
		        COALESCE(r.avatar_url, ri.room_avatar_url),
		        COALESCE(r.default_avatar_key, ri.room_default_avatar_key),
		        COALESCE(r.visibility, ri.room_visibility),
		        COALESCE(r.join_policy, ri.room_join_policy),
		        CASE WHEN r.id IS NULL THEN 0 ELSE 1 END
		 FROM room_invites ri
		 JOIN users u ON u.id = ri.inviter_user_id
		 LEFT JOIN rooms r ON r.id = ri.room_id
		 LEFT JOIN room_memberships irm ON irm.room_id = ri.room_id AND irm.user_id = ri.inviter_user_id
		 WHERE ri.id = ?`,
		inviteID,
	).Scan(
		&id, &roomID, &status, &createdAt, &updatedAt,
		&inviterID, &inviterUID, &inviterUsername, &inviterDisplayName, &inviterAvatarURL, &inviterDefaultAvatar,
		&inviterRoomDisplayName, &inviterRoomRole,
		&rid, &name, &avatarURL, &defaultAvatar, &visibility, &joinPolicy, &roomExists,
	)
	if err != nil {
		return gin.H{"id": inviteID}
	}
	memberCount := 0
	liveCount := 0
	if roomExists != 0 {
		memberCount, _ = h.memberCount(roomID)
		_, liveCount, _ = h.livePreview(roomID)
	}
	var joinedCount int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, viewerID).Scan(&joinedCount)
	joined := joinedCount > 0
	inviter := summaryFromUserFields(
		inviterID,
		inviterUID,
		inviterUsername,
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
		invalidReason = h.roomInviteInvalidReason(roomID, inviterID)
	}
	return gin.H{
		"id": id, "status": status, "created_at": formatMillis(createdAt), "updated_at": formatMillis(updatedAt),
		"room_exists": roomExists != 0, "invalid_reason": nullableStringFromText(invalidReason),
		"room": gin.H{
			"id": roomID, "rid": rid, "name": name,
			"avatar_url": nullableString(avatarURL), "default_avatar_key": defaultAvatar,
			"visibility": visibility, "join_policy": joinPolicy,
			"member_count": memberCount, "live_participant_count": liveCount,
			"joined": joined, "join_state": h.joinState(roomID, viewerID, joined),
		},
		"inviter": inviter,
	}
}

func (h *Handler) roomInviteInvalidReason(roomID, inviterID string) string {
	if !h.roomIDExists(roomID) {
		return "room_missing"
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
	var reviewerID, reviewerUID, reviewerUsername sql.NullString
	var reviewerDisplayName, reviewerAvatarURL, reviewerDefaultAvatar, reviewerRoomDisplayName, reviewerRoomRole sql.NullString
	var reviewedAt sql.NullInt64
	var rid, name, defaultAvatar, visibility, joinPolicy string
	var avatarURL sql.NullString
	err := h.DB.QueryRow(
		`SELECT jr.id, jr.room_id, jr.status, jr.reason, jr.created_at, jr.updated_at,
		        jr.reviewer_user_id, jr.reviewed_at,
		        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
		        rrm.room_display_name, rrm.role,
		        r.rid, r.name, r.avatar_url, r.default_avatar_key, r.visibility, r.join_policy
		 FROM join_requests jr
		 JOIN rooms r ON r.id = jr.room_id
		 LEFT JOIN users u ON u.id = jr.reviewer_user_id
		 LEFT JOIN room_memberships rrm ON rrm.room_id = jr.room_id AND rrm.user_id = jr.reviewer_user_id
		 WHERE jr.id = ? AND jr.user_id = ?`,
		requestID, viewerID,
	).Scan(
		&id, &roomID, &status, &reason, &createdAt, &updatedAt,
		&reviewerID, &reviewedAt,
		&reviewerID, &reviewerUID, &reviewerUsername, &reviewerDisplayName, &reviewerAvatarURL, &reviewerDefaultAvatar,
		&reviewerRoomDisplayName, &reviewerRoomRole,
		&rid, &name, &avatarURL, &defaultAvatar, &visibility, &joinPolicy,
	)
	if err != nil {
		return gin.H{"id": requestID}
	}

	memberCount, _ := h.memberCount(roomID)
	onlineCount, _ := h.onlineMemberCount(roomID)
	_, liveCount, _ := h.livePreview(roomID)
	var joinedCount int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, viewerID).Scan(&joinedCount)
	joined := joinedCount > 0

	var reviewer any
	if reviewerID.Valid && reviewerID.String != "" && reviewerUsername.Valid {
		summary := summaryFromUserFields(
			reviewerID.String,
			reviewerUID.String,
			reviewerUsername.String,
			reviewerDisplayName,
			reviewerAvatarURL,
			reviewerDefaultAvatar,
		)
		summary.RoomDisplayName = nullableString(reviewerRoomDisplayName)
		if reviewerRoomRole.Valid && reviewerRoomRole.String != "" {
			summary.RoomRole = reviewerRoomRole.String
		} else if h.isSuperuser(reviewerID.String) {
			summary.RoomRole = "superuser"
		}
		reviewer = summary
	}

	return gin.H{
		"id": id, "status": status, "reason": reason, "created_at": formatMillis(createdAt), "updated_at": formatMillis(updatedAt),
		"reviewed_at": nullableMillis(reviewedAt),
		"room": gin.H{
			"id": roomID, "rid": rid, "name": name,
			"avatar_url": nullableString(avatarURL), "default_avatar_key": defaultAvatar,
			"visibility": visibility, "join_policy": joinPolicy,
			"member_count": memberCount, "online_member_count": onlineCount, "live_participant_count": liveCount,
			"joined": joined, "join_state": h.joinState(roomID, viewerID, joined),
		},
		"reviewer": reviewer,
	}
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

func (h *Handler) currentRoomProfileAvatarKey(roomID, userID string) string {
	var key sql.NullString
	_ = h.DB.QueryRow(`SELECT room_default_avatar_key FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&key)
	if key.Valid {
		return key.String
	}
	return ""
}
