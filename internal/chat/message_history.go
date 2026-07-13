package chat

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const messageHistoryMaxBatch = 100

func (h *Handler) ensureMessageHistorySchema() error {
	_, err := h.DB.Exec(
		`CREATE TABLE IF NOT EXISTS message_history_deletions (
			user_id VARCHAR(128) NOT NULL,
			message_id VARCHAR(128) NOT NULL,
			created_at BIGINT NOT NULL,
			PRIMARY KEY (user_id, message_id),
			INDEX idx_message_history_deletions_message (message_id)
		)`,
	)
	return err
}

func (h *Handler) listMessageHistory(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	userID := currentUserID(c)
	if h.roomMessagesBlocked(roomID, userID) {
		c.JSON(http.StatusOK, gin.H{"messages": []message{}, "has_more": false, "next_before": nil})
		return
	}

	conditions := []string{
		"m.room_id = ?",
		visibleMessageSQL("m"),
		"NOT EXISTS (SELECT 1 FROM message_history_deletions mhd WHERE mhd.user_id = ? AND mhd.message_id = m.id)",
	}
	args := []any{roomID, userID}

	if query := strings.TrimSpace(c.Query("query")); query != "" {
		pattern := "%" + strings.ToLower(query) + "%"
		conditions = append(conditions, `(
			LOWER(m.body) LIKE ? OR LOWER(m.attachments_json) LIKE ? OR
			LOWER(u.username) LIKE ? OR LOWER(COALESCE(u.display_name, '')) LIKE ? OR
			LOWER(COALESCE(sender_rm.room_display_name, '')) LIKE ?
		)`)
		args = append(args, pattern, pattern, pattern, pattern, pattern)
	}

	if senderUserID := strings.TrimSpace(c.Query("sender_user_id")); senderUserID != "" {
		conditions = append(conditions, "m.sender_user_id = ?")
		args = append(args, senderUserID)
	}

	category := strings.ToLower(strings.TrimSpace(c.DefaultQuery("category", "all")))
	categoryCondition, ok := messageHistoryCategoryCondition(category)
	if !ok {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid message history category")
		return
	}
	if categoryCondition != "" {
		conditions = append(conditions, categoryCondition)
	}

	if startAt, ok := h.parseMessageHistoryTime(c, "start_at"); !ok {
		return
	} else if startAt != nil {
		conditions = append(conditions, "m.created_at >= ?")
		args = append(args, *startAt)
	}
	if endAt, ok := h.parseMessageHistoryTime(c, "end_at"); !ok {
		return
	} else if endAt != nil {
		conditions = append(conditions, "m.created_at < ?")
		args = append(args, *endAt)
	}

	if before := strings.TrimSpace(c.Query("before")); before != "" {
		var beforeCreatedAt int64
		err := h.DB.QueryRow(
			`SELECT created_at FROM messages WHERE id = ? AND room_id = ?`,
			before,
			roomID,
		).Scan(&beforeCreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			h.jsonError(c, http.StatusBadRequest, "bad_request", "before message does not exist")
			return
		}
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read message history cursor")
			return
		}
		conditions = append(conditions, "(m.created_at < ? OR (m.created_at = ? AND m.id < ?))")
		args = append(args, beforeCreatedAt, beforeCreatedAt, before)
	}

	limit := parseLimit(c.Query("limit"), 50, 100)
	args = append(args, limit+1)
	rows, err := h.DB.Query(
		`SELECT m.id, m.room_id, m.client_message_id, m.type, m.body,
		        m.mentions_json, m.attachments_json, m.is_recalled, m.recalled_at,
		        m.recalled_by_user_id, m.is_force_deleted, m.force_deleted_at,
		        m.force_deleted_by_user_id, m.created_at,
		        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
		        u.is_superuser, sender_rm.room_display_name,
		        CASE WHEN u.is_superuser != 0 THEN 'superuser' ELSE COALESCE(sender_rm.role, '') END
		 FROM messages m
		 JOIN users u ON u.id = m.sender_user_id
		 LEFT JOIN room_memberships sender_rm ON sender_rm.room_id = m.room_id AND sender_rm.user_id = m.sender_user_id
		 WHERE `+strings.Join(conditions, " AND ")+`
		 ORDER BY m.created_at DESC, m.id DESC
		 LIMIT ?`,
		args...,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to list message history")
		return
	}
	defer rows.Close()

	messages := make([]message, 0, limit+1)
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read message history")
			return
		}
		messages = append(messages, h.messageForViewer(msg, userID))
	}
	if err := rows.Err(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read message history")
		return
	}

	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}
	var nextBefore *string
	if hasMore && len(messages) > 0 {
		id := messages[len(messages)-1].ID
		nextBefore = &id
	}
	c.JSON(http.StatusOK, gin.H{"messages": messages, "has_more": hasMore, "next_before": nextBefore})
}

func messageHistoryCategoryCondition(category string) (string, bool) {
	const imageAttachment = `EXISTS (
		SELECT 1 FROM JSON_TABLE(m.attachments_json, '$[*]' COLUMNS (
			attachment_type VARCHAR(64) PATH '$.type' NULL ON EMPTY,
			mime_type VARCHAR(255) PATH '$.asset.mime_type' NULL ON EMPTY
		)) history_attachment
		WHERE LOWER(COALESCE(history_attachment.attachment_type, '')) = 'file'
		  AND LOWER(COALESCE(history_attachment.mime_type, '')) LIKE 'image/%'
	)`
	const fileAttachment = `EXISTS (
		SELECT 1 FROM JSON_TABLE(m.attachments_json, '$[*]' COLUMNS (
			attachment_type VARCHAR(64) PATH '$.type' NULL ON EMPTY,
			mime_type VARCHAR(255) PATH '$.asset.mime_type' NULL ON EMPTY
		)) history_attachment
		WHERE LOWER(COALESCE(history_attachment.attachment_type, '')) = 'file'
		  AND LOWER(COALESCE(history_attachment.mime_type, '')) NOT LIKE 'image/%'
	)`
	switch category {
	case "all":
		return "", true
	case "links":
		return `m.type = 'text' AND LOWER(m.body) REGEXP '(https?://|www\\.)'`, true
	case "stickers":
		return `m.type = 'sticker'`, true
	case "images":
		return imageAttachment, true
	case "files":
		return fileAttachment, true
	case "system":
		return `m.type = 'system'`, true
	default:
		return "", false
	}
}

func (h *Handler) parseMessageHistoryTime(c *gin.Context, name string) (*int64, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil, true
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", name+" must be RFC3339")
		return nil, false
	}
	millis := value.UnixMilli()
	return &millis, true
}

func (h *Handler) hideMessageHistory(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	var req struct {
		MessageIDs []string `json:"message_ids"`
		Confirm    bool     `json:"confirm"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || !req.Confirm {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "message_ids and confirm are required")
		return
	}
	messageIDs := uniqueNonEmptyStrings(req.MessageIDs)
	if len(messageIDs) == 0 || len(messageIDs) > messageHistoryMaxBatch {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "message_ids must contain 1 to 100 items")
		return
	}

	tx, err := h.DB.Begin()
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to delete message history")
		return
	}
	defer tx.Rollback()
	userID := currentUserID(c)
	now := nowMillis()
	for _, messageID := range messageIDs {
		var exists int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM messages WHERE id = ? AND room_id = ?`,
			messageID,
			roomID,
		).Scan(&exists); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to delete message history")
			return
		}
		if exists == 0 {
			h.jsonError(c, http.StatusNotFound, "not_found", "message not found")
			return
		}
		if _, err := tx.Exec(
			`INSERT IGNORE INTO message_history_deletions (user_id, message_id, created_at) VALUES (?, ?, ?)`,
			userID,
			messageID,
			now,
		); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to delete message history")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to delete message history")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "deleted_count": len(messageIDs)})
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
