package model

import "database/sql"

// EnsureRoomAIVoicePreferencesSchema moves the legacy room-wide AI voice
// announcement switch into a per-user, per-room preference. Existing members
// inherit the room's previous value exactly once; the legacy room value is
// then normalized to false so members added later receive the new default.
func EnsureRoomAIVoicePreferencesSchema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS room_ai_voice_preferences (
		room_id VARCHAR(128) NOT NULL,
		user_id VARCHAR(128) NOT NULL,
		enabled TINYINT(1) NOT NULL DEFAULT 0,
		updated_at BIGINT NOT NULL,
		PRIMARY KEY (room_id, user_id),
		CONSTRAINT fk_room_ai_voice_preferences_room
			FOREIGN KEY (room_id) REFERENCES rooms(id) ON DELETE CASCADE,
		CONSTRAINT fk_room_ai_voice_preferences_user
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT IGNORE INTO room_ai_voice_preferences (room_id, user_id, enabled, updated_at)
		SELECT rm.room_id, rm.user_id, r.ai_voice_announce_enabled, r.updated_at
		FROM room_memberships rm
		JOIN rooms r ON r.id = rm.room_id`); err != nil {
		return err
	}
	_, err := db.Exec(`UPDATE rooms SET ai_voice_announce_enabled = 0 WHERE ai_voice_announce_enabled != 0`)
	return err
}
