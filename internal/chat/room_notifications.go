package chat

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	roomNotificationMemberRemoved          = "member_removed"
	roomNotificationRolePromoted           = "role_promoted"
	roomNotificationRoleDemoted            = "role_demoted"
	roomNotificationCreatorTransferDemoted = "creator_transfer_demoted"
	roomNotificationMentioned              = "mentioned"
	defaultRoomNotificationListLimit       = 100
	maxRoomNotificationListLimit           = 200
)

type roomNotificationSpec struct {
	Type              string
	RecipientID       string
	RoomID            string
	ActorID           string
	FromRole          string
	ToRole            string
	MessageID         string
	MessagePreview    string
	NotificationLevel string
}

func roomRoleNotificationType(fromRole, toRole string) string {
	if roleRank(toRole) > roleRank(fromRole) {
		return roomNotificationRolePromoted
	}
	return roomNotificationRoleDemoted
}

func (h *Handler) appendRoomNotificationTx(tx *sql.Tx, spec roomNotificationSpec) error {
	if spec.Type == "" || spec.RecipientID == "" || spec.RoomID == "" {
		return nil
	}
	if !h.roomNotificationAllowedTx(tx, spec) {
		return nil
	}

	var rid, roomName, roomDefaultAvatar, roomVisibility, roomJoinPolicy, roomDescription string
	var roomAvatarURL, roomCreatedByUserID sql.NullString
	if err := tx.QueryRow(
		`SELECT COALESCE(rid, ''), name, avatar_url,
		        COALESCE(default_avatar_key, 'room-1'),
		        COALESCE(visibility, 'private'),
		        COALESCE(join_policy, 'closed'),
		        COALESCE(description, ''),
		        created_by_user_id
		 FROM rooms
		 WHERE id = ?`,
		spec.RoomID,
	).Scan(
		&rid, &roomName, &roomAvatarURL, &roomDefaultAvatar, &roomVisibility,
		&roomJoinPolicy, &roomDescription, &roomCreatedByUserID,
	); err != nil {
		return err
	}

	var actorUID, actorUsername, actorDisplayName, actorAvatarURL, actorDefaultAvatar sql.NullString
	var actorRoomDisplayName, actorRoomRole sql.NullString
	actorDefaultAvatarValue := "blue-3"
	if spec.ActorID != "" {
		if err := tx.QueryRow(
			`SELECT u.uid, u.username, u.display_name, u.avatar_url,
			        u.default_avatar_key, rm.room_display_name, rm.role
			 FROM users u
			 LEFT JOIN room_memberships rm ON rm.room_id = ? AND rm.user_id = u.id
			 WHERE u.id = ?`,
			spec.RoomID, spec.ActorID,
		).Scan(
			&actorUID, &actorUsername, &actorDisplayName, &actorAvatarURL,
			&actorDefaultAvatar, &actorRoomDisplayName, &actorRoomRole,
		); err != nil {
			return err
		}
		if !actorDefaultAvatar.Valid || actorDefaultAvatar.String == "" {
			actorDefaultAvatar = sql.NullString{String: "blue-3", Valid: true}
		}
		actorDefaultAvatarValue = actorDefaultAvatar.String
	}

	_, err := tx.Exec(
		`INSERT INTO room_notifications (
		   id, recipient_user_id, room_id, actor_user_id, type, from_role, to_role,
		   created_at, room_rid, room_name, room_avatar_url, room_default_avatar_key,
		   room_visibility, room_join_policy, room_description, room_created_by_user_id,
		   actor_uid, actor_username, actor_display_name, actor_avatar_url,
		   actor_default_avatar_key, actor_room_display_name, actor_room_role,
		   message_id, message_body_preview
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newID("rnot"),
		spec.RecipientID,
		spec.RoomID,
		emptyToNil(spec.ActorID),
		spec.Type,
		emptyToNil(spec.FromRole),
		emptyToNil(spec.ToRole),
		nowMillis(),
		rid,
		roomName,
		nullableString(roomAvatarURL),
		roomDefaultAvatar,
		roomVisibility,
		roomJoinPolicy,
		roomDescription,
		nullableString(roomCreatedByUserID),
		nullableString(actorUID),
		nullableString(actorUsername),
		nullableString(actorDisplayName),
		nullableString(actorAvatarURL),
		actorDefaultAvatarValue,
		nullableString(actorRoomDisplayName),
		nullableString(actorRoomRole),
		emptyToNil(spec.MessageID),
		emptyToNil(spec.MessagePreview),
	)
	return err
}

func (h *Handler) appendMentionRoomNotifications(roomID, messageID, body, mentionsJSON, actorID string) ([]string, error) {
	rows, err := h.DB.Query(
		`SELECT user_id, COALESCE(notification_level, 'all')
		 FROM room_memberships
		 WHERE room_id = ? AND user_id != ?`,
		roomID,
		actorID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type candidate struct {
		userID string
		level  string
	}
	candidates := make([]candidate, 0)
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.userID, &item.level); err != nil {
			return nil, err
		}
		if item.level == "all" {
			candidates = append(candidates, item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	tx, err := h.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	recipients := make([]string, 0, len(candidates))
	preview := lastMessageBodyPreview("text", body, "[]")
	for _, item := range candidates {
		target, ok := h.mentionTarget(roomID, item.userID)
		if !ok || !mentionMessageTargetsUser(body, mentionsJSON, target) {
			continue
		}
		if err := h.appendRoomNotificationTx(tx, roomNotificationSpec{
			Type:              roomNotificationMentioned,
			RecipientID:       item.userID,
			RoomID:            roomID,
			ActorID:           actorID,
			MessageID:         messageID,
			MessagePreview:    preview,
			NotificationLevel: item.level,
		}); err != nil {
			return nil, err
		}
		recipients = append(recipients, item.userID)
	}
	if len(recipients) == 0 {
		return nil, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return recipients, nil
}

func (h *Handler) deleteRoomNotificationsForMessage(roomID, messageID string) []string {
	if roomID == "" || messageID == "" {
		return nil
	}
	rows, err := h.DB.Query(
		`SELECT DISTINCT recipient_user_id
		 FROM room_notifications
		 WHERE room_id = ? AND message_id = ?`,
		roomID,
		messageID,
	)
	if err != nil {
		return nil
	}
	recipients := make([]string, 0)
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err == nil && userID != "" {
			recipients = append(recipients, userID)
		}
	}
	_ = rows.Close()
	_, _ = h.DB.Exec(
		`DELETE FROM room_notifications
		 WHERE room_id = ? AND message_id = ?`,
		roomID,
		messageID,
	)
	return recipients
}

func (h *Handler) roomNotificationAllowedTx(tx *sql.Tx, spec roomNotificationSpec) bool {
	level := strings.TrimSpace(spec.NotificationLevel)
	if level == "" {
		_ = tx.QueryRow(
			`SELECT COALESCE(notification_level, 'all')
			 FROM room_memberships
			 WHERE room_id = ? AND user_id = ?`,
			spec.RoomID,
			spec.RecipientID,
		).Scan(&level)
	}
	return level == "all"
}

func (h *Handler) ensureRoomNotificationSchema() error {
	if _, err := h.DB.Exec(
		`CREATE TABLE IF NOT EXISTS room_notification_deletions (
			user_id VARCHAR(128) NOT NULL,
			notification_type VARCHAR(32) NOT NULL,
			notification_id VARCHAR(128) NOT NULL,
			created_at BIGINT NOT NULL,
			PRIMARY KEY (user_id, notification_type, notification_id)
		)`,
	); err != nil {
		return err
	}
	if err := h.ensureRoomNotificationColumn("message_id", "VARCHAR(128) NULL"); err != nil {
		return err
	}
	if err := h.ensureRoomNotificationColumn("message_body_preview", "TEXT NULL"); err != nil {
		return err
	}
	return nil
}

func (h *Handler) ensureRoomNotificationColumn(name, definition string) error {
	var count int
	if err := h.DB.QueryRow(
		`SELECT COUNT(*)
		 FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE()
		   AND TABLE_NAME = 'room_notifications'
		   AND COLUMN_NAME = ?`,
		name,
	).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err := h.DB.Exec(`ALTER TABLE room_notifications ADD COLUMN ` + name + ` ` + definition)
	return err
}

func (h *Handler) listRoomNotifications(c *gin.Context) {
	userID := currentUserID(c)
	limit := parseLimit(c.Query("limit"), defaultRoomNotificationListLimit, maxRoomNotificationListLimit)
	rows, err := h.DB.Query(
		`SELECT id
		 FROM room_notifications
		 WHERE recipient_user_id = ?
		   AND NOT EXISTS (
			   SELECT 1
			   FROM room_notification_deletions rnd
			   WHERE rnd.user_id = room_notifications.recipient_user_id
			     AND rnd.notification_type = 'room_event'
			     AND rnd.notification_id = room_notifications.id
		   )
		 ORDER BY created_at DESC
		 LIMIT ?`,
		userID, limit,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "list room notifications failed")
		return
	}
	defer rows.Close()

	items := make([]gin.H, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "read room notification failed")
			return
		}
		items = append(items, h.roomEventNotificationPayload(id, userID))
	}
	if err := rows.Err(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "read room notifications failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"notifications": items, "next_cursor": nil})
}

const (
	roomNotificationDeletionInvite               = "invite"
	roomNotificationDeletionApplicationRequested = "application_requested"
	roomNotificationDeletionApplicationReviewed  = "application_reviewed"
	roomNotificationDeletionRoomEvent            = "room_event"
)

func (h *Handler) deleteRoomNotification(c *gin.Context) {
	userID := currentUserID(c)
	notificationType := strings.TrimSpace(c.Param("notification_type"))
	notificationID := strings.TrimSpace(c.Param("notification_id"))
	if notificationID == "" || !allowed(
		notificationType,
		roomNotificationDeletionInvite,
		roomNotificationDeletionApplicationRequested,
		roomNotificationDeletionApplicationReviewed,
		roomNotificationDeletionRoomEvent,
	) {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid room notification")
		return
	}

	var count int
	switch notificationType {
	case roomNotificationDeletionInvite:
		_ = h.DB.QueryRow(
			`SELECT COUNT(*) FROM room_invites WHERE id = ? AND target_user_id = ?`,
			notificationID,
			userID,
		).Scan(&count)
	case roomNotificationDeletionApplicationRequested, roomNotificationDeletionApplicationReviewed:
		_ = h.DB.QueryRow(
			`SELECT COUNT(*) FROM join_requests WHERE id = ? AND user_id = ?`,
			notificationID,
			userID,
		).Scan(&count)
	case roomNotificationDeletionRoomEvent:
		_ = h.DB.QueryRow(
			`SELECT COUNT(*) FROM room_notifications WHERE id = ? AND recipient_user_id = ?`,
			notificationID,
			userID,
		).Scan(&count)
	}
	if count == 0 {
		h.jsonError(c, http.StatusNotFound, "not_found", "room notification not found")
		return
	}

	if _, err := h.DB.Exec(
		`INSERT INTO room_notification_deletions (
			user_id, notification_type, notification_id, created_at
		 ) VALUES (?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE created_at = created_at`,
		userID,
		notificationType,
		notificationID,
		nowMillis(),
	); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "delete room notification failed")
		return
	}

	switch notificationType {
	case roomNotificationDeletionInvite:
		h.publishRoomInvitesUpdated(userID)
	case roomNotificationDeletionApplicationRequested, roomNotificationDeletionApplicationReviewed:
		h.publishRoomApplicationsUpdated(userID)
	case roomNotificationDeletionRoomEvent:
		h.publishRoomNotificationsUpdated(userID)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) roomNotificationDeleted(
	userID, notificationType, notificationID string,
) bool {
	var count int
	if err := h.DB.QueryRow(
		`SELECT COUNT(*)
		 FROM room_notification_deletions
		 WHERE user_id = ? AND notification_type = ? AND notification_id = ?`,
		userID,
		notificationType,
		notificationID,
	).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

func (h *Handler) markRoomNotificationsRead(c *gin.Context) {
	userID := currentUserID(c)
	if _, err := h.DB.Exec(
		`UPDATE room_notifications
		 SET read_at = ?
		 WHERE recipient_user_id = ? AND read_at IS NULL`,
		nowMillis(), userID,
	); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "mark room notifications read failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) roomEventNotificationPayload(notificationID, viewerID string) gin.H {
	var id, eventType, roomID string
	var actorRefID, fromRole, toRole sql.NullString
	var messageID, messagePreview sql.NullString
	var readAt sql.NullInt64
	var createdAt int64
	var rid, roomName, roomDefaultAvatar, roomVisibility, roomJoinPolicy, roomDescription string
	var roomAvatarURL, roomCreatedByUserID sql.NullString
	var roomExists int
	var actorID, actorUID, actorUsername sql.NullString
	var actorDisplayName, actorAvatarURL, actorDefaultAvatar sql.NullString
	var actorRoomDisplayName, actorRoomRole sql.NullString
	err := h.DB.QueryRow(
		`SELECT rn.id, rn.type, rn.room_id, rn.actor_user_id, rn.from_role, rn.to_role,
		        rn.message_id, rn.message_body_preview,
		        rn.created_at, rn.read_at,
		        CASE WHEN rn.room_name != '' THEN rn.room_rid ELSE COALESCE(r.rid, '') END,
		        CASE WHEN rn.room_name != '' THEN rn.room_name ELSE COALESCE(r.name, '') END,
		        CASE WHEN rn.room_name != '' THEN rn.room_avatar_url ELSE r.avatar_url END,
		        CASE WHEN rn.room_name != '' THEN rn.room_default_avatar_key ELSE COALESCE(r.default_avatar_key, 'room-1') END,
		        CASE WHEN rn.room_name != '' THEN rn.room_visibility ELSE COALESCE(r.visibility, 'private') END,
		        CASE WHEN rn.room_name != '' THEN rn.room_join_policy ELSE COALESCE(r.join_policy, 'closed') END,
		        CASE WHEN rn.room_name != '' THEN rn.room_description ELSE COALESCE(r.description, '') END,
		        CASE WHEN rn.room_name != '' THEN rn.room_created_by_user_id ELSE r.created_by_user_id END,
		        CASE WHEN r.id IS NULL THEN 0 ELSE 1 END,
		        u.id,
		        CASE WHEN COALESCE(rn.actor_username, '') != '' THEN rn.actor_uid ELSE u.uid END,
		        CASE WHEN COALESCE(rn.actor_username, '') != '' THEN rn.actor_username ELSE u.username END,
		        CASE WHEN COALESCE(rn.actor_username, '') != '' THEN rn.actor_display_name ELSE u.display_name END,
		        CASE WHEN COALESCE(rn.actor_username, '') != '' THEN rn.actor_avatar_url ELSE u.avatar_url END,
		        CASE WHEN COALESCE(rn.actor_username, '') != '' THEN rn.actor_default_avatar_key ELSE u.default_avatar_key END,
		        CASE WHEN COALESCE(rn.actor_username, '') != '' THEN rn.actor_room_display_name ELSE rm.room_display_name END,
		        CASE WHEN COALESCE(rn.actor_username, '') != '' THEN rn.actor_room_role ELSE rm.role END
		 FROM room_notifications rn
		 LEFT JOIN rooms r ON r.id = rn.room_id
		 LEFT JOIN users u ON u.id = rn.actor_user_id
		 LEFT JOIN room_memberships rm ON rm.room_id = rn.room_id AND rm.user_id = rn.actor_user_id
		 WHERE rn.id = ? AND rn.recipient_user_id = ?`,
		notificationID, viewerID,
	).Scan(
		&id, &eventType, &roomID, &actorRefID, &fromRole, &toRole,
		&messageID, &messagePreview, &createdAt, &readAt,
		&rid, &roomName, &roomAvatarURL, &roomDefaultAvatar, &roomVisibility,
		&roomJoinPolicy, &roomDescription, &roomCreatedByUserID, &roomExists,
		&actorID, &actorUID, &actorUsername, &actorDisplayName, &actorAvatarURL,
		&actorDefaultAvatar, &actorRoomDisplayName, &actorRoomRole,
	)
	if err != nil {
		return gin.H{"id": notificationID}
	}
	if !messageID.Valid || strings.TrimSpace(messageID.String) == "" {
		if resolvedMessageID, ok := h.resolveRoomNotificationMessageID(
			id,
			eventType,
			roomID,
			viewerID,
			actorRefID,
			fromRole,
			toRole,
			createdAt,
		); ok {
			messageID = sql.NullString{String: resolvedMessageID, Valid: true}
		}
	}

	var actor any
	actorExists := true
	if actorRefID.Valid && actorRefID.String != "" {
		actorExists = actorID.Valid && actorUsername.Valid
		if actorExists {
			summary := summaryFromUserFields(
				actorID.String,
				actorUID.String,
				actorUsername.String,
				actorDisplayName,
				actorAvatarURL,
				actorDefaultAvatar,
			)
			summary.RoomDisplayName = nullableString(actorRoomDisplayName)
			if actorRoomRole.Valid && actorRoomRole.String != "" {
				summary.RoomRole = actorRoomRole.String
			} else if h.isSuperuser(actorID.String) {
				summary.RoomRole = "superuser"
			}
			summary.IsSuperuser = h.isSuperuser(actorID.String)
			actor = summary
		} else {
			actor = deletedUserSummary(actorRefID.String)
		}
	}

	return gin.H{
		"id":              id,
		"type":            eventType,
		"created_at":      formatMillis(createdAt),
		"read_at":         nullableMillis(readAt),
		"from_role":       nullableString(fromRole),
		"to_role":         nullableString(toRole),
		"message_id":      nullableString(messageID),
		"message_preview": nullableString(messagePreview),
		"room_exists":     roomExists != 0,
		"actor_exists":    actorExists,
		"room": h.roomNotificationRoomPayload(
			roomID, viewerID, rid, roomName, roomDefaultAvatar, roomVisibility,
			roomJoinPolicy, roomAvatarURL,
			sql.NullString{String: roomDescription, Valid: roomDescription != ""},
			roomCreatedByUserID, roomExists != 0,
		),
		"actor": actor,
	}
}

func (h *Handler) resolveRoomNotificationMessageID(
	notificationID string,
	notificationType string,
	roomID string,
	recipientID string,
	actorID sql.NullString,
	fromRole sql.NullString,
	toRole sql.NullString,
	notificationCreatedAt int64,
) (string, bool) {
	systemEvent, ok := roomNotificationSystemEvent(notificationType)
	if !ok || roomID == "" || recipientID == "" {
		return "", false
	}

	conditions := []string{
		"m.room_id = ?",
		"m.type = ?",
		"m.is_recalled = 0",
		"m.is_force_deleted = 0",
		"JSON_UNQUOTE(JSON_EXTRACT(m.attachments_json, '$[0].type')) = ?",
		"JSON_UNQUOTE(JSON_EXTRACT(m.attachments_json, '$[0].event')) = ?",
		"JSON_UNQUOTE(JSON_EXTRACT(m.attachments_json, '$[0].target.id')) = ?",
	}
	args := []any{
		roomID,
		systemMessageType,
		systemMessageType,
		systemEvent,
		recipientID,
	}
	if actorID.Valid && strings.TrimSpace(actorID.String) != "" {
		conditions = append(
			conditions,
			"JSON_UNQUOTE(JSON_EXTRACT(m.attachments_json, '$[0].actor.id')) = ?",
		)
		args = append(args, actorID.String)
	}
	if notificationType == roomNotificationRolePromoted ||
		notificationType == roomNotificationRoleDemoted ||
		notificationType == roomNotificationCreatorTransferDemoted {
		if !fromRole.Valid || !toRole.Valid {
			return "", false
		}
		conditions = append(
			conditions,
			"JSON_UNQUOTE(JSON_EXTRACT(m.attachments_json, '$[0].from_role')) = ?",
			"JSON_UNQUOTE(JSON_EXTRACT(m.attachments_json, '$[0].to_role')) = ?",
		)
		args = append(args, fromRole.String, toRole.String)
	}
	if notificationCreatedAt > 0 {
		conditions = append(
			conditions,
			"m.created_at BETWEEN ? AND ?",
		)
		args = append(
			args,
			notificationCreatedAt-10*60*1000,
			notificationCreatedAt+10*60*1000,
		)
	}

	query := `SELECT m.id
	 FROM messages m
	 WHERE ` + strings.Join(conditions, " AND ") + `
	 ORDER BY ABS(m.created_at - ?) ASC
	 LIMIT 1`
	args = append(args, notificationCreatedAt)

	var messageID string
	if err := h.DB.QueryRow(query, args...).Scan(&messageID); err != nil {
		return "", false
	}
	_, _ = h.DB.Exec(
		`UPDATE room_notifications
		 SET message_id = ?
		 WHERE id = ? AND (message_id IS NULL OR message_id = '')`,
		messageID,
		notificationID,
	)
	return messageID, true
}

func roomNotificationSystemEvent(notificationType string) (string, bool) {
	switch notificationType {
	case roomNotificationMemberRemoved:
		return systemEventRoomMemberRemoved, true
	case roomNotificationRolePromoted,
		roomNotificationRoleDemoted,
		roomNotificationCreatorTransferDemoted:
		return systemEventRoomRoleChanged, true
	default:
		return "", false
	}
}
