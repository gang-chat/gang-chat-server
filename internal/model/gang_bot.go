package model

import (
	"database/sql"
	"fmt"
)

// EnsureGangBotSchema adds the per-room gang-bot switch. Rooms have the bot
// on until an admin turns it off.
func EnsureGangBotSchema(db *sql.DB) error {
	var columnCount int
	if err := db.QueryRow(
		`SELECT COUNT(*)
		 FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE()
		   AND TABLE_NAME = 'rooms'
		   AND COLUMN_NAME = 'gang_bot_enabled'`,
	).Scan(&columnCount); err != nil {
		return fmt.Errorf("inspect rooms.gang_bot_enabled: %w", err)
	}
	if columnCount != 0 {
		return nil
	}
	if _, err := db.Exec(
		`ALTER TABLE rooms
		 ADD COLUMN gang_bot_enabled TINYINT(1) NOT NULL DEFAULT 1 AFTER ai_voice_announce_enabled`,
	); err != nil {
		return fmt.Errorf("add rooms.gang_bot_enabled: %w", err)
	}
	return nil
}
