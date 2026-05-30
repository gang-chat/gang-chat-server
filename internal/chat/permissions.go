package chat

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/model"
)

func (h *Handler) requireMember(c *gin.Context, roomID string) bool {
	if h.isRoomMember(roomID, currentUserID(c)) {
		return true
	}
	h.jsonError(c, http.StatusNotFound, "not_found", "room not found")
	return false
}

func (h *Handler) requireRoomAccess(c *gin.Context, roomID string) bool {
	userID := currentUserID(c)
	if h.isRoomMember(roomID, userID) {
		return true
	}
	if h.isSuperuser(userID) && h.roomIDExists(roomID) {
		return true
	}
	h.jsonError(c, http.StatusNotFound, "not_found", "room not found")
	return false
}

func (h *Handler) isRoomMember(roomID, userID string) bool {
	var exists int
	_ = h.DB.QueryRow(
		`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`,
		roomID, userID,
	).Scan(&exists)
	return exists != 0
}

func (h *Handler) roomExists(c *gin.Context, roomID string) bool {
	if h.roomIDExists(roomID) {
		return true
	}
	h.jsonError(c, http.StatusNotFound, "not_found", "room not found")
	return false
}

func (h *Handler) roomIDExists(roomID string) bool {
	var exists int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM rooms WHERE id = ?`, roomID).Scan(&exists)
	return exists != 0
}

func (h *Handler) validateMentions(c *gin.Context, roomID string, mentions []any) bool {
	for _, item := range mentions {
		m, ok := item.(map[string]any)
		if !ok {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid mention")
			return false
		}
		kind, _ := m["type"].(string)
		if kind == "all" {
			continue
		}
		if kind != "user" {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid mention type")
			return false
		}
		userID, _ := m["user_id"].(string)
		if userID == "" {
			if uid, _ := m["uid"].(string); uid != "" {
				_ = h.DB.QueryRow(`SELECT id FROM users WHERE uid = ?`, uid).Scan(&userID)
			}
		}
		var exists int
		_ = h.DB.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&exists)
		if exists == 0 {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "mentioned user is not in the room")
			return false
		}
	}
	return true
}

func (h *Handler) isTextMuted(roomID, userID string) bool {
	var mutedUntil sql.NullInt64
	if err := h.DB.QueryRow(`SELECT text_muted_until FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&mutedUntil); err != nil {
		return false
	}
	return mutedUntil.Valid && (mutedUntil.Int64 == 0 || mutedUntil.Int64 > nowMillis())
}

func (h *Handler) joinState(roomID, userID string, joined bool) string {
	if joined {
		return "joined"
	}
	var status string
	_ = h.DB.QueryRow(`SELECT status FROM join_requests WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&status)
	if status == "pending" {
		return "pending"
	}
	return "none"
}

func (h *Handler) isAdmin(roomID, userID string) bool {
	if h.isSuperuser(userID) {
		return h.roomIDExists(roomID)
	}
	var role string
	_ = h.DB.QueryRow(`SELECT role FROM room_memberships WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&role)
	return role == "owner" || role == "admin"
}

func (h *Handler) isSuperuser(userID string) bool {
	return model.IsSuperuser(h.DB, userID)
}

func (h *Handler) isProtectedSuperuserTarget(actorID, targetID string) bool {
	return actorID != targetID && h.isSuperuser(targetID) && !h.isSuperuser(actorID)
}
