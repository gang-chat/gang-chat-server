package model

import (
	"database/sql"
	"fmt"
)

// EnsureLiveScreenViewerSchema adds the per-participant screen being watched.
// The relation is intentionally stored on live_participants: a viewer can only
// watch one staged screen at a time, and the row already owns the viewer's
// room-scoped live lifecycle.
func EnsureLiveScreenViewerSchema(db *sql.DB) error {
	var columnCount int
	if err := db.QueryRow(
		`SELECT COUNT(*)
		 FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE()
		   AND TABLE_NAME = 'live_participants'
		   AND COLUMN_NAME = 'watching_screen_user_id'`,
	).Scan(&columnCount); err != nil {
		return fmt.Errorf("inspect live_participants.watching_screen_user_id: %w", err)
	}
	if columnCount == 0 {
		if _, err := db.Exec(
			`ALTER TABLE live_participants
			 ADD COLUMN watching_screen_user_id VARCHAR(128) NULL AFTER screen_sharing`,
		); err != nil {
			return fmt.Errorf("add live screen viewer target: %w", err)
		}
	}

	var indexCount int
	if err := db.QueryRow(
		`SELECT COUNT(*)
		 FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE()
		   AND TABLE_NAME = 'live_participants'
		   AND INDEX_NAME = 'idx_live_participants_watching_screen'`,
	).Scan(&indexCount); err != nil {
		return fmt.Errorf("inspect live screen viewer index: %w", err)
	}
	if indexCount == 0 {
		if _, err := db.Exec(
			`ALTER TABLE live_participants
			 ADD INDEX idx_live_participants_watching_screen
			   (room_id, watching_screen_user_id, connection_state)`,
		); err != nil {
			return fmt.Errorf("add live screen viewer index: %w", err)
		}
	}
	return nil
}
