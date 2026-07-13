package chat

import (
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const messageHistoryMaxBatch = 100
const messageHistoryScanBatch = 100

var messageHistoryLinkPattern = regexp.MustCompile(`(?i)(https?://|www\.)`)

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
	if !validMessageHistoryCategory(category) {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid message history category")
		return
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

	var cursorCreatedAt *int64
	cursorID := ""
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
		cursorCreatedAt = &beforeCreatedAt
		cursorID = before
	}

	limit := parseLimit(c.Query("limit"), 50, 100)
	messages := make([]message, 0, limit+1)
	for len(messages) <= limit {
		queryConditions := append([]string(nil), conditions...)
		queryArgs := append([]any(nil), args...)
		if cursorCreatedAt != nil {
			queryConditions = append(queryConditions, "(m.created_at < ? OR (m.created_at = ? AND m.id < ?))")
			queryArgs = append(queryArgs, *cursorCreatedAt, *cursorCreatedAt, cursorID)
		}
		queryArgs = append(queryArgs, messageHistoryScanBatch)
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
			 WHERE `+strings.Join(queryConditions, " AND ")+`
			 ORDER BY m.created_at DESC, m.id DESC
			 LIMIT ?`,
			queryArgs...,
		)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to list message history")
			return
		}

		scanned := 0
		var lastScanned message
		for rows.Next() {
			msg, err := scanMessage(rows)
			if err != nil {
				rows.Close()
				h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read message history")
				return
			}
			scanned++
			lastScanned = msg
			if messageMatchesHistoryCategory(msg, category) {
				messages = append(messages, h.messageForViewer(msg, userID))
			}
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read message history")
			return
		}
		if len(messages) > limit || scanned < messageHistoryScanBatch {
			break
		}
		createdAt, err := time.Parse(time.RFC3339Nano, lastScanned.CreatedAt)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read message history cursor")
			return
		}
		createdAtMillis := createdAt.UnixMilli()
		cursorCreatedAt = &createdAtMillis
		cursorID = lastScanned.ID
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

func validMessageHistoryCategory(category string) bool {
	switch category {
	case "all", "links", "voice", "stickers", "images", "files", "system":
		return true
	default:
		return false
	}
}

func messageMatchesHistoryCategory(msg message, category string) bool {
	if category == "all" {
		return true
	}
	if category == "links" {
		return msg.Type == "text" && messageHistoryLinkPattern.MatchString(msg.Body)
	}
	if category == "system" {
		return msg.Type == "system"
	}

	hasVoice := messageHasVoiceAttachment(msg)
	if category == "voice" {
		return hasVoice
	}
	if category == "stickers" {
		if msg.Type == "sticker" {
			return true
		}
		for _, raw := range msg.Attachments {
			attachment, ok := raw.(map[string]any)
			if ok && strings.EqualFold(stringFromMap(attachment, "type"), "sticker") {
				return true
			}
		}
		return false
	}
	if hasVoice {
		return false
	}

	for _, raw := range msg.Attachments {
		attachment, ok := raw.(map[string]any)
		if !ok || !strings.EqualFold(stringFromMap(attachment, "type"), "file") {
			continue
		}
		isImage := strings.HasPrefix(strings.ToLower(attachmentMimeType(attachment)), "image/")
		if category == "images" && isImage {
			return true
		}
		if category == "files" && !isImage {
			return true
		}
	}
	return false
}

func messageHasVoiceAttachment(msg message) bool {
	messageIsAudio := strings.EqualFold(msg.Type, "audio")
	for _, raw := range msg.Attachments {
		attachment, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		attachmentType := strings.ToLower(stringFromMap(attachment, "type"))
		mimeType := strings.ToLower(attachmentMimeType(attachment))
		name := strings.ToLower(strings.ReplaceAll(attachmentDisplayName(attachment), `\`, "/"))
		if separator := strings.LastIndex(name, "/"); separator >= 0 {
			name = name[separator+1:]
		}
		legacyVoiceFile := attachmentType == "file" && strings.HasPrefix(mimeType, "audio/") && strings.HasPrefix(name, "voice_")
		_, hasDuration := attachment["duration_ms"]
		_, hasAsset := attachment["asset"].(map[string]any)
		if (messageIsAudio || attachmentType == "audio" || legacyVoiceFile) && (hasAsset || hasDuration) {
			return true
		}
	}
	return false
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
