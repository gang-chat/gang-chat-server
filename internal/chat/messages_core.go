package chat

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

func (h *Handler) listMessages(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	limit := parseLimit(c.Query("limit"), 50, 100)

	var rows *sql.Rows
	var err error
	before := c.Query("before")
	if before == "" {
		rows, err = h.DB.Query(
			`SELECT m.id, m.room_id, m.client_message_id, m.type, m.body,
			        m.mentions_json, m.attachments_json, m.is_recalled, m.recalled_at,
			        m.recalled_by_user_id, m.is_force_deleted, m.force_deleted_at,
			        m.force_deleted_by_user_id, m.created_at,
			        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key
			 FROM messages m
			 JOIN users u ON u.id = m.sender_user_id
			 WHERE m.room_id = ?
			 ORDER BY m.created_at DESC
			 LIMIT ?`,
			roomID, limit,
		)
	} else {
		var beforeCreatedAt int64
		err = h.DB.QueryRow(`SELECT created_at FROM messages WHERE id = ? AND room_id = ?`, before, roomID).Scan(&beforeCreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			h.jsonError(c, http.StatusBadRequest, "bad_request", "before message does not exist")
			return
		}
		if err == nil {
			rows, err = h.DB.Query(
				`SELECT m.id, m.room_id, m.client_message_id, m.type, m.body,
				        m.mentions_json, m.attachments_json, m.is_recalled, m.recalled_at,
				        m.recalled_by_user_id, m.is_force_deleted, m.force_deleted_at,
				        m.force_deleted_by_user_id, m.created_at,
				        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key
				 FROM messages m
				 JOIN users u ON u.id = m.sender_user_id
				 WHERE m.room_id = ? AND m.created_at < ?
				 ORDER BY m.created_at DESC
				 LIMIT ?`,
				roomID, beforeCreatedAt, limit,
			)
		}
	}
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to list messages")
		return
	}
	defer rows.Close()

	messages := make([]message, 0)
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read messages")
			return
		}
		messages = append(messages, msg)
	}
	reverseMessages(messages)

	hasMore := false
	var nextBefore *string
	if len(messages) > 0 {
		firstID := messages[0].ID
		firstCreatedAt := parseRFC3339Millis(messages[0].CreatedAt)
		var count int
		_ = h.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE room_id = ? AND created_at < ?`, roomID, firstCreatedAt).Scan(&count)
		hasMore = count > 0
		if hasMore {
			nextBefore = &firstID
		}
	}

	c.JSON(http.StatusOK, gin.H{"messages": messages, "has_more": hasMore, "next_before": nextBefore})
}

func (h *Handler) sendMessage(c *gin.Context) {
	roomID := c.Param("room_id")
	userID := currentUserID(c)
	if !h.requireMember(c, roomID) {
		return
	}

	var req sendMessageRequest
	rawBody, ok := h.bindJSON(c, &req)
	if !ok {
		return
	}
	if h.replayIdempotency(c, rawBody) {
		return
	}
	body := strings.TrimRight(req.Body, "\r\n")
	messageType := req.Type
	if messageType == "" {
		messageType = "text"
	}
	if !allowed(messageType, "text", "sticker", "audio", "file") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid message type")
		return
	}
	if req.ClientMessageID == "" || utf8.RuneCountInString(body) > 4000 {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "client_message_id is required")
		return
	}
	if messageType == "text" && strings.TrimSpace(body) == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "message body is required")
		return
	}
	if messageType != "text" && len(req.Attachments) == 0 {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "attachments are required")
		return
	}
	if h.isTextMuted(roomID, userID) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "user is muted in this room")
		return
	}
	if !h.validateMentions(c, roomID, req.Mentions) {
		return
	}
	mentionsJSON := mustJSON(req.Mentions)
	attachmentsJSON := mustJSON(req.Attachments)

	now := nowMillis()
	messageID := newID("msg")
	_, err := h.DB.Exec(
		`INSERT INTO messages (id, room_id, sender_user_id, client_message_id, type, body, mentions_json, attachments_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		messageID, roomID, userID, req.ClientMessageID, messageType, body, mentionsJSON, attachmentsJSON, now,
	)
	if err != nil {
		existing, existingErr := h.messageByClientID(roomID, userID, req.ClientMessageID)
		if existingErr == nil {
			h.idempotentJSON(c, http.StatusCreated, rawBody, gin.H{"message": existing})
			return
		}
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to send message")
		return
	}
	_, _ = h.DB.Exec(`UPDATE rooms SET updated_at = ? WHERE id = ?`, now, roomID)

	msg, err := h.messageByID(messageID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read message")
		return
	}
	// last_message lives in the room-list snapshot, so a new message refreshes
	// every member's list entry. Clients use the new last_message to bump their
	// own unread counter; the count itself is never on the wire (it's personal).
	h.publishRoomUpdated(roomID)
	h.idempotentJSON(c, http.StatusCreated, rawBody, gin.H{"message": msg})
}

func (h *Handler) markRead(c *gin.Context) {
	roomID := c.Param("room_id")
	userID := currentUserID(c)
	if !h.requireRoomAccess(c, roomID) {
		return
	}

	var req markReadRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.LastReadMessageID == "" {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "last_read_message_id is required")
		return
	}
	var exists int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE id = ? AND room_id = ?`, req.LastReadMessageID, roomID).Scan(&exists); err != nil || exists == 0 {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "message does not exist")
		return
	}

	_, err := h.DB.Exec(
		`INSERT INTO room_reads (room_id, user_id, last_read_message_id, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(room_id, user_id) DO UPDATE SET
		   last_read_message_id = excluded.last_read_message_id,
		   updated_at = excluded.updated_at`,
		roomID, userID, req.LastReadMessageID, nowMillis(),
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to mark room read")
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true, "unread_count": h.unreadCount(roomID, userID)})
}

func (h *Handler) messageByID(messageID string) (message, error) {
	return h.queryMessage(
		`SELECT m.id, m.room_id, m.client_message_id, m.type, m.body,
		        m.mentions_json, m.attachments_json, m.is_recalled, m.recalled_at,
		        m.recalled_by_user_id, m.is_force_deleted, m.force_deleted_at,
		        m.force_deleted_by_user_id, m.created_at,
		        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key
		 FROM messages m
		 JOIN users u ON u.id = m.sender_user_id
		 WHERE m.id = ?`,
		messageID,
	)
}

func (h *Handler) messageByClientID(roomID, userID, clientMessageID string) (message, error) {
	return h.queryMessage(
		`SELECT m.id, m.room_id, m.client_message_id, m.type, m.body,
		        m.mentions_json, m.attachments_json, m.is_recalled, m.recalled_at,
		        m.recalled_by_user_id, m.is_force_deleted, m.force_deleted_at,
		        m.force_deleted_by_user_id, m.created_at,
		        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key
		 FROM messages m
		 JOIN users u ON u.id = m.sender_user_id
		 WHERE m.room_id = ? AND m.sender_user_id = ? AND m.client_message_id = ?`,
		roomID, userID, clientMessageID,
	)
}

func (h *Handler) queryMessage(query string, args ...any) (message, error) {
	var msg message
	var senderID, senderUID, senderUsername string
	var senderDisplayName, senderAvatarURL, senderDefaultAvatar sql.NullString
	var mentionsJSON, attachmentsJSON string
	var recalledAt, forceDeletedAt sql.NullInt64
	var recalledByUserID, forceDeletedByUserID sql.NullString
	var isRecalled, isForceDeleted int
	var createdAt int64
	err := h.DB.QueryRow(query, args...).Scan(
		&msg.ID, &msg.RoomID, &msg.ClientMessageID, &msg.Type, &msg.Body,
		&mentionsJSON, &attachmentsJSON, &isRecalled, &recalledAt, &recalledByUserID,
		&isForceDeleted, &forceDeletedAt, &forceDeletedByUserID, &createdAt,
		&senderID, &senderUID, &senderUsername, &senderDisplayName, &senderAvatarURL, &senderDefaultAvatar,
	)
	if err != nil {
		return message{}, err
	}
	msg.Sender = summaryFromUserFields(senderID, senderUID, senderUsername, senderDisplayName, senderAvatarURL, senderDefaultAvatar)
	msg.Mentions = decodeJSONArray(mentionsJSON)
	msg.Attachments = decodeJSONArray(attachmentsJSON)
	msg.IsRecalled = isRecalled != 0
	msg.IsForceDeleted = isForceDeleted != 0
	if recalledAt.Valid {
		v := formatMillis(recalledAt.Int64)
		msg.RecalledAt = &v
	}
	if recalledByUserID.Valid {
		if summary, err := h.userSummary(recalledByUserID.String); err == nil {
			msg.RecalledBy = &summary
		}
	}
	if forceDeletedAt.Valid {
		v := formatMillis(forceDeletedAt.Int64)
		msg.ForceDeletedAt = &v
	}
	if forceDeletedByUserID.Valid {
		if summary, err := h.userSummary(forceDeletedByUserID.String); err == nil {
			msg.ForceDeletedBy = &summary
		}
	}
	msg.CreatedAt = formatMillis(createdAt)
	return msg, nil
}
