package chat

import "database/sql"

type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func (h *Handler) aiVoiceAnnouncementsEnabled(roomID, userID string) bool {
	if roomID == "" || userID == "" {
		return false
	}
	var enabled int
	err := h.DB.QueryRow(
		`SELECT enabled FROM room_ai_voice_preferences WHERE room_id = ? AND user_id = ?`,
		roomID,
		userID,
	).Scan(&enabled)
	return err == nil && enabled != 0
}

func upsertAIVoiceAnnouncementsPreference(execer sqlExecer, roomID, userID string, enabled bool, updatedAt int64) error {
	_, err := execer.Exec(
		`INSERT INTO room_ai_voice_preferences (room_id, user_id, enabled, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE enabled = VALUES(enabled), updated_at = VALUES(updated_at)`,
		roomID,
		userID,
		boolToInt(enabled),
		updatedAt,
	)
	return err
}
