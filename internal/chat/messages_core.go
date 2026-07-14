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
	userID := currentUserID(c)
	if h.roomMessagesBlocked(roomID, userID) {
		c.JSON(http.StatusOK, gin.H{"messages": []message{}, "has_more": false, "next_before": nil})
		return
	}
	limit := parseLimit(c.Query("limit"), 50, 100)

	var rows *sql.Rows
	var err error
	before := c.Query("before")
	if before == "" {
		rows, err = h.DB.Query(
			`SELECT `+messageSelectColumnsSQL+`
			 FROM messages m
			 LEFT JOIN users u ON u.id = m.sender_user_id
			 LEFT JOIN room_memberships sender_rm ON sender_rm.room_id = m.room_id AND sender_rm.user_id = m.sender_user_id
			 WHERE m.room_id = ? AND `+visibleMessageSQL("m")+`
			 ORDER BY m.created_at DESC, m.id DESC
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
				`SELECT `+messageSelectColumnsSQL+`
				 FROM messages m
				 LEFT JOIN users u ON u.id = m.sender_user_id
				 LEFT JOIN room_memberships sender_rm ON sender_rm.room_id = m.room_id AND sender_rm.user_id = m.sender_user_id
				 WHERE m.room_id = ?
				   AND (m.created_at < ? OR (m.created_at = ? AND m.id < ?))
				   AND `+visibleMessageSQL("m")+`
				 ORDER BY m.created_at DESC, m.id DESC
				 LIMIT ?`,
				roomID, beforeCreatedAt, beforeCreatedAt, before, limit,
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
		msg = h.messageForViewer(msg, userID)
		messages = append(messages, msg)
	}
	reverseMessages(messages)

	hasMore := false
	var nextBefore *string
	if len(messages) > 0 {
		firstID := messages[0].ID
		firstCreatedAt := parseRFC3339Millis(messages[0].CreatedAt)
		var count int
		_ = h.DB.QueryRow(
			`SELECT COUNT(*) FROM messages m
			 WHERE m.room_id = ?
			   AND (m.created_at < ? OR (m.created_at = ? AND m.id < ?))
			   AND `+visibleMessageSQL("m"),
			roomID,
			firstCreatedAt,
			firstCreatedAt,
			firstID,
		).Scan(&count)
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
	// requireRoomAccess lets a superuser ghost (no membership row) post for
	// announcements/moderation; their message carries their normal sender
	// identity. Normal non-members still get 404.
	if !h.requireRoomAccess(c, roomID) {
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
	_, err := insertMessageWithSenderSnapshot(
		h.DB,
		messageID, roomID, userID, req.ClientMessageID, messageType, body,
		mentionsJSON, attachmentsJSON, now,
	)
	if err != nil {
		existing, existingErr := h.messageByClientIDForUser(roomID, userID, req.ClientMessageID, userID)
		if existingErr == nil {
			h.idempotentJSON(c, http.StatusCreated, rawBody, gin.H{"message": existing})
			return
		}
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to send message")
		return
	}
	_, _ = h.DB.Exec(`UPDATE rooms SET updated_at = ? WHERE id = ?`, now, roomID)

	msg, err := h.messageByIDForUser(messageID, userID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read message")
		return
	}
	// last_message lives in the room-list snapshot, so a new message refreshes
	// every member's list entry. Personal unread counts are added separately for
	// each recipient while the shared snapshot is published.
	h.publishRoomMessageUpdated(roomID, userID)
	h.publishRoomToUser(userID, roomID, "room_updated")
	if len(req.Mentions) > 0 {
		if recipients, err := h.appendMentionRoomNotifications(
			roomID,
			messageID,
			body,
			mentionsJSON,
			userID,
		); err == nil {
			for _, recipientID := range recipients {
				h.publishRoomNotificationsUpdated(recipientID)
			}
		}
	}
	h.idempotentJSON(c, http.StatusCreated, rawBody, gin.H{"message": msg})
}

func (h *Handler) markRead(c *gin.Context) {
	roomID := c.Param("room_id")
	userID := currentUserID(c)
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	if h.roomMessagesBlocked(roomID, userID) {
		c.JSON(http.StatusOK, gin.H{"ok": true, "unread_count": 0})
		return
	}

	var req markReadRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.LastReadMessageID == "" {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "last_read_message_id is required")
		return
	}
	var candidateCreatedAt int64
	if err := h.DB.QueryRow(
		`SELECT created_at FROM messages WHERE id = ? AND room_id = ?`,
		req.LastReadMessageID,
		roomID,
	).Scan(&candidateCreatedAt); errors.Is(err, sql.ErrNoRows) {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "message does not exist")
		return
	} else if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read message")
		return
	}

	now := nowMillis()
	// Ensure the per-account cursor exists first, then conditionally advance it.
	// The UPDATE predicate is evaluated while MySQL holds the room_reads row
	// lock, so concurrent devices can race safely: an older cursor can never
	// overwrite a newer one. The message id is the deterministic tie-breaker
	// used everywhere messages share the same millisecond timestamp.
	_, err := h.DB.Exec(
		`INSERT INTO room_reads (room_id, user_id, last_read_message_id, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE updated_at = updated_at`,
		roomID, userID, req.LastReadMessageID, now,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to mark room read")
		return
	}
	_, err = h.DB.Exec(
		`UPDATE room_reads rr
		 LEFT JOIN messages current_message ON current_message.id = rr.last_read_message_id
		 SET rr.last_read_message_id = ?, rr.updated_at = ?
		 WHERE rr.room_id = ? AND rr.user_id = ?
		   AND (
		     current_message.id IS NULL
		     OR current_message.created_at < ?
		     OR (current_message.created_at = ? AND current_message.id < ?)
		   )`,
		req.LastReadMessageID,
		now,
		roomID,
		userID,
		candidateCreatedAt,
		candidateCreatedAt,
		req.LastReadMessageID,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to mark room read")
		return
	}

	unreadCount := h.unreadCount(roomID, userID)
	unreadMentionCount := h.unreadMentionCount(roomID, userID)
	// room_updated is account-addressed, so every live connection for this
	// user receives the committed cursor-derived counts, including the device
	// that initiated the request.
	h.publishRoomToUser(userID, roomID, "room_updated")
	c.JSON(http.StatusOK, gin.H{
		"ok":                   true,
		"unread_count":         unreadCount,
		"unread_mention_count": unreadMentionCount,
	})
}

func (h *Handler) messageByID(messageID string) (message, error) {
	return h.queryMessage(
		`SELECT `+messageSelectColumnsSQL+`
		 FROM messages m
		 LEFT JOIN users u ON u.id = m.sender_user_id
		 LEFT JOIN room_memberships sender_rm ON sender_rm.room_id = m.room_id AND sender_rm.user_id = m.sender_user_id
		 WHERE m.id = ?`,
		messageID,
	)
}

func (h *Handler) messageByIDForUser(messageID, viewerID string) (message, error) {
	msg, err := h.messageByID(messageID)
	if err != nil {
		return message{}, err
	}
	return h.messageForViewer(msg, viewerID), nil
}

func (h *Handler) messageByClientID(roomID, userID, clientMessageID string) (message, error) {
	return h.queryMessage(
		`SELECT `+messageSelectColumnsSQL+`
		 FROM messages m
		 LEFT JOIN users u ON u.id = m.sender_user_id
		 LEFT JOIN room_memberships sender_rm ON sender_rm.room_id = m.room_id AND sender_rm.user_id = m.sender_user_id
		 WHERE m.room_id = ? AND m.sender_user_id = ? AND m.client_message_id = ?`,
		roomID, userID, clientMessageID,
	)
}

func (h *Handler) messageByClientIDForUser(roomID, userID, clientMessageID, viewerID string) (message, error) {
	msg, err := h.messageByClientID(roomID, userID, clientMessageID)
	if err != nil {
		return message{}, err
	}
	return h.messageForViewer(msg, viewerID), nil
}

func (h *Handler) messageForViewer(msg message, viewerID string) message {
	h.hydrateMessageActionUsers(&msg)
	if !msg.IsRecalled || msg.Type != "text" {
		return msg
	}
	if viewerID != "" && h.canRecallMemberMessage(msg.RoomID, viewerID, msg.Sender.ID) {
		return msg
	}
	msg.Body = ""
	return msg
}

func (h *Handler) hydrateMessageActionUsers(msg *message) {
	if msg == nil {
		return
	}
	if msg.RecalledBy == nil && msg.recalledByUserID != "" {
		if summary, err := h.userSummaryForRoom(msg.RoomID, msg.recalledByUserID); err == nil {
			msg.RecalledBy = &summary
		}
	}
	if msg.ForceDeletedBy == nil && msg.forceDeletedByUserID != "" {
		if summary, err := h.userSummaryForRoom(msg.RoomID, msg.forceDeletedByUserID); err == nil {
			msg.ForceDeletedBy = &summary
		}
	}
}

func (h *Handler) queryMessage(query string, args ...any) (message, error) {
	var msg message
	var senderID, senderUID, senderUsername string
	var senderDisplayName, senderAvatarURL, senderDefaultAvatar sql.NullString
	var senderRoomDisplayName, senderRoomRole sql.NullString
	var mentionsJSON, attachmentsJSON string
	var recalledAt, forceDeletedAt sql.NullInt64
	var recalledByUserID, forceDeletedByUserID sql.NullString
	var isRecalled, isForceDeleted, senderIsSuperuser, senderIsDeleted int
	var createdAt int64
	err := h.DB.QueryRow(query, args...).Scan(
		&msg.ID, &msg.RoomID, &msg.ClientMessageID, &msg.Type, &msg.Body,
		&mentionsJSON, &attachmentsJSON, &isRecalled, &recalledAt, &recalledByUserID,
		&isForceDeleted, &forceDeletedAt, &forceDeletedByUserID, &createdAt,
		&senderID, &senderUID, &senderUsername, &senderDisplayName, &senderAvatarURL, &senderDefaultAvatar,
		&senderIsSuperuser, &senderRoomDisplayName, &senderRoomRole, &senderIsDeleted,
	)
	if err != nil {
		return message{}, err
	}
	msg.Sender = summaryFromUserFields(senderID, senderUID, senderUsername, senderDisplayName, senderAvatarURL, senderDefaultAvatar)
	msg.Sender.IsSuperuser = senderIsSuperuser != 0
	msg.Sender.IsDeleted = senderIsDeleted != 0
	msg.Sender.RoomDisplayName = nullableString(senderRoomDisplayName)
	if senderRoomRole.Valid && senderRoomRole.String != "" {
		msg.Sender.RoomRole = senderRoomRole.String
	}
	msg.Mentions = decodeJSONArray(mentionsJSON)
	msg.Attachments = decodeJSONArray(attachmentsJSON)
	msg.IsRecalled = isRecalled != 0
	msg.IsForceDeleted = isForceDeleted != 0
	if recalledAt.Valid {
		v := formatMillis(recalledAt.Int64)
		msg.RecalledAt = &v
	}
	if recalledByUserID.Valid {
		msg.recalledByUserID = recalledByUserID.String
	}
	if forceDeletedAt.Valid {
		v := formatMillis(forceDeletedAt.Int64)
		msg.ForceDeletedAt = &v
	}
	if forceDeletedByUserID.Valid {
		msg.forceDeletedByUserID = forceDeletedByUserID.String
	}
	msg.CreatedAt = formatMillis(createdAt)
	return msg, nil
}
