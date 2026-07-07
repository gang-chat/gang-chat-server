package chat

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	roomNotificationMemberRemoved           = "member_removed"
	roomNotificationRolePromoted            = "role_promoted"
	roomNotificationRoleDemoted             = "role_demoted"
	roomNotificationCreatorTransferDemoted  = "creator_transfer_demoted"
	roomNotificationMentioned               = "mentioned"
	defaultRoomNotificationListLimit        = 100
	maxRoomNotificationListLimit            = 200
	missingRoomNotificationActorDisplayName = "用户不存在"
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
		        COALESCE(r.rid, rn.room_rid),
		        COALESCE(r.name, rn.room_name),
		        COALESCE(r.avatar_url, rn.room_avatar_url),
		        COALESCE(r.default_avatar_key, rn.room_default_avatar_key),
		        COALESCE(r.visibility, rn.room_visibility),
		        COALESCE(r.join_policy, rn.room_join_policy),
		        COALESCE(r.description, rn.room_description),
		        COALESCE(r.created_by_user_id, rn.room_created_by_user_id),
		        CASE WHEN r.id IS NULL THEN 0 ELSE 1 END,
		        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
		        COALESCE(rm.room_display_name, rn.actor_room_display_name),
		        COALESCE(rm.role, rn.actor_room_role)
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
			actor = summaryFromUserFields(
				actorRefID.String,
				"",
				"",
				sql.NullString{String: missingRoomNotificationActorDisplayName, Valid: true},
				sql.NullString{},
				sql.NullString{String: "graphite-2", Valid: true},
			)
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
