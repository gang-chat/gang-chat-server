package chat

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
)

func (h *Handler) recallMessage(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	var req reasonRequest
	if !h.bindOptionalJSON(c, &req) {
		return
	}
	messageID := c.Param("message_id")
	userID := currentUserID(c)
	var senderID, policy string
	var createdAt int64
	var window sql.NullInt64
	err := h.DB.QueryRow(
		`SELECT m.sender_user_id, m.created_at, r.message_recall_policy, r.message_recall_window_seconds
		 FROM messages m JOIN rooms r ON r.id = m.room_id
		 WHERE m.id = ? AND m.room_id = ?`,
		messageID, roomID,
	).Scan(&senderID, &createdAt, &policy, &window)
	if err != nil {
		h.jsonError(c, http.StatusNotFound, "not_found", "message not found")
		return
	}
	admin := h.isAdmin(roomID, userID)
	if !admin && senderID != userID {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot recall another user's message")
		return
	}
	if !admin && policy == "disabled" {
		h.jsonError(c, http.StatusForbidden, "forbidden", "message recall disabled")
		return
	}
	if !admin && policy == "admin_approval" {
		requestID := newID("mrr")
		now := nowMillis()
		_, _ = h.DB.Exec(
			`INSERT INTO message_recall_requests (id, room_id, message_id, requested_by_user_id, status, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'pending', ?, ?)
			 ON CONFLICT(room_id, message_id, requested_by_user_id) DO UPDATE SET updated_at = excluded.updated_at`,
			requestID, roomID, messageID, userID, now, now,
		)
		c.JSON(http.StatusAccepted, gin.H{"recall_request": gin.H{"id": requestID, "message_id": messageID, "room_id": roomID, "status": "pending", "created_at": formatMillis(now)}})
		return
	}
	if !admin && window.Valid && window.Int64 >= 0 && nowMillis()-createdAt > window.Int64*1000 {
		h.jsonError(c, http.StatusForbidden, "forbidden", "message recall window expired")
		return
	}
	h.applyRecall(roomID, messageID, userID)
	msg, _ := h.messageByID(messageID)
	c.JSON(http.StatusOK, gin.H{"message": msg})
}

func (h *Handler) listMessageRecallRequests(c *gin.Context) {
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
		`SELECT id, message_id, requested_by_user_id, status, created_at FROM message_recall_requests WHERE room_id = ? AND status = ? ORDER BY created_at ASC`,
		roomID, status,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "list recall requests failed")
		return
	}
	defer rows.Close()
	requests := make([]gin.H, 0)
	for rows.Next() {
		var id, messageID, requestedBy, requestStatus string
		var createdAt int64
		if err := rows.Scan(&id, &messageID, &requestedBy, &requestStatus, &createdAt); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "read recall request failed")
			return
		}
		requests = append(requests, gin.H{"id": id, "message_id": messageID, "room_id": roomID, "requested_by_user_id": requestedBy, "status": requestStatus, "created_at": formatMillis(createdAt)})
	}
	c.JSON(http.StatusOK, gin.H{"requests": requests, "next_cursor": nil})
}

func (h *Handler) reviewMessageRecallRequest(c *gin.Context) {
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
	var messageID string
	if err := h.DB.QueryRow(`SELECT message_id FROM message_recall_requests WHERE id = ? AND room_id = ?`, c.Param("request_id"), roomID).Scan(&messageID); err != nil {
		h.jsonError(c, http.StatusNotFound, "not_found", "recall request not found")
		return
	}
	status := "rejected"
	if req.Decision == "approve" {
		status = "approved"
		h.applyRecall(roomID, messageID, currentUserID(c))
	}
	_, _ = h.DB.Exec(`UPDATE message_recall_requests SET status = ?, updated_at = ? WHERE id = ?`, status, nowMillis(), c.Param("request_id"))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) forceDeleteMessage(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.isAdmin(roomID, currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	var req confirmRequest
	if err := c.ShouldBindJSON(&req); err != nil || !req.Confirm {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "confirm must be true")
		return
	}
	res, err := h.DB.Exec(
		`UPDATE messages SET body = '', mentions_json = '[]', attachments_json = '[]',
		        is_force_deleted = 1, force_deleted_at = ?, force_deleted_by_user_id = ?
		 WHERE id = ? AND room_id = ?`,
		nowMillis(), currentUserID(c), c.Param("message_id"), roomID,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "force delete failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.jsonError(c, http.StatusNotFound, "not_found", "message not found")
		return
	}
	msg, _ := h.messageByID(c.Param("message_id"))
	// Force-deleting the latest message changes the last_message preview.
	h.publishRoomUpdated(roomID)
	c.JSON(http.StatusOK, gin.H{"message": msg})
}

func (h *Handler) applyRecall(roomID, messageID, userID string) {
	_, _ = h.DB.Exec(
		`UPDATE messages SET body = '', mentions_json = '[]', attachments_json = '[]',
		        is_recalled = 1, recalled_at = ?, recalled_by_user_id = ?
		 WHERE id = ? AND room_id = ?`,
		nowMillis(), userID, messageID, roomID,
	)
	// Recalling the latest message changes the room's last_message preview, so
	// refresh every member's list entry. Covers both the immediate-recall and
	// the admin-approval paths since both funnel through here.
	h.publishRoomUpdated(roomID)
}
