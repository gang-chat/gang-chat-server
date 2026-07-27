package model

import (
	"database/sql"
	"fmt"
)

// EnsureLiveCameraMirrorSchema adds the per-live-session camera mirror flag.
// The flag belongs to the participant row because it only affects the camera
// currently published in that voice session and resets on the next join.
func EnsureLiveCameraMirrorSchema(db *sql.DB) error {
	var columnCount int
	if err := db.QueryRow(
		`SELECT COUNT(*)
		 FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE()
		   AND TABLE_NAME = 'live_participants'
		   AND COLUMN_NAME = 'camera_mirrored'`,
	).Scan(&columnCount); err != nil {
		return fmt.Errorf("inspect live_participants.camera_mirrored: %w", err)
	}
	if columnCount != 0 {
		return nil
	}
	if _, err := db.Exec(
		`ALTER TABLE live_participants
		 ADD COLUMN camera_mirrored TINYINT(1) NOT NULL DEFAULT 0 AFTER camera_on`,
	); err != nil {
		return fmt.Errorf("add live camera mirror state: %w", err)
	}
	return nil
}
