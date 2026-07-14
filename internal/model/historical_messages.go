package model

import (
	"database/sql"
	"fmt"
	"strings"
)

// SQLExecer is implemented by both *sql.DB and *sql.Tx.
type SQLExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

type messageSnapshotColumn struct {
	name       string
	definition string
}

var messageSnapshotColumns = []messageSnapshotColumn{
	{name: "sender_uid_snapshot", definition: "VARCHAR(32) NOT NULL DEFAULT ''"},
	{name: "sender_username_snapshot", definition: "VARCHAR(64) NOT NULL DEFAULT ''"},
	{name: "sender_display_name_snapshot", definition: "VARCHAR(128) NULL"},
	{name: "sender_room_display_name_snapshot", definition: "VARCHAR(128) NULL"},
	{name: "sender_avatar_url_snapshot", definition: "TEXT NULL"},
	{name: "sender_default_avatar_key_snapshot", definition: "VARCHAR(64) NOT NULL DEFAULT 'blue-3'"},
	{name: "sender_is_superuser_snapshot", definition: "TINYINT(1) NOT NULL DEFAULT 0"},
	{name: "sender_room_role_snapshot", definition: "VARCHAR(32) NOT NULL DEFAULT ''"},
}

// EnsureHistoricalMessageRetentionSchema upgrades existing installations so
// deleting an account cannot cascade into historical messages or assets used
// by those messages. It is intentionally idempotent because both startup and
// the destructive account action call it as a safety gate.
func EnsureHistoricalMessageRetentionSchema(db *sql.DB) error {
	for _, column := range messageSnapshotColumns {
		if err := ensureMessageSnapshotColumn(db, column); err != nil {
			return err
		}
	}
	if err := BackfillMessageSenderSnapshots(db, ""); err != nil {
		return fmt.Errorf("backfill message sender snapshots: %w", err)
	}
	if err := dropForeignKeysForColumn(db, "messages", "sender_user_id", "users"); err != nil {
		return fmt.Errorf("detach message senders from users: %w", err)
	}
	if err := ensureAssetOwnerSurvivesUserDeletion(db); err != nil {
		return fmt.Errorf("retain historical assets: %w", err)
	}
	return nil
}

func ensureMessageSnapshotColumn(db *sql.DB, column messageSnapshotColumn) error {
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*)
		 FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'messages' AND COLUMN_NAME = ?`,
		column.name,
	).Scan(&count); err != nil {
		return fmt.Errorf("inspect messages.%s: %w", column.name, err)
	}
	if count > 0 {
		return nil
	}
	_, err := db.Exec(
		"ALTER TABLE messages ADD COLUMN " + quoteMySQLIdentifier(column.name) + " " + column.definition,
	)
	if err != nil {
		return fmt.Errorf("add messages.%s: %w", column.name, err)
	}
	return nil
}

// BackfillMessageSenderSnapshots captures the best available identity for old
// rows created before snapshots existed. New messages are snapshotted at insert
// time and are never overwritten here.
func BackfillMessageSenderSnapshots(execer SQLExecer, userID string) error {
	where := "m.sender_username_snapshot = ''"
	args := make([]any, 0, 1)
	if strings.TrimSpace(userID) != "" {
		where += " AND m.sender_user_id = ?"
		args = append(args, userID)
	}
	_, err := execer.Exec(
		`UPDATE messages m
		 JOIN users u ON u.id = m.sender_user_id
		 LEFT JOIN room_memberships rm
		   ON rm.room_id = m.room_id AND rm.user_id = m.sender_user_id
		 SET m.sender_uid_snapshot = COALESCE(u.uid, ''),
		     m.sender_username_snapshot = u.username,
		     m.sender_display_name_snapshot = u.display_name,
		     m.sender_room_display_name_snapshot = rm.room_display_name,
		     m.sender_avatar_url_snapshot = u.avatar_url,
		     m.sender_default_avatar_key_snapshot = COALESCE(NULLIF(u.default_avatar_key, ''), 'blue-3'),
		     m.sender_is_superuser_snapshot = u.is_superuser,
		     m.sender_room_role_snapshot = CASE
		       WHEN u.is_superuser != 0 THEN 'superuser'
		       ELSE COALESCE(rm.role, '')
		     END
		 WHERE `+where,
		args...,
	)
	return err
}

func dropForeignKeysForColumn(db *sql.DB, table, column, referencedTable string) error {
	rows, err := db.Query(
		`SELECT DISTINCT CONSTRAINT_NAME
		 FROM information_schema.KEY_COLUMN_USAGE
		 WHERE TABLE_SCHEMA = DATABASE()
		   AND TABLE_NAME = ?
		   AND COLUMN_NAME = ?
		   AND REFERENCED_TABLE_NAME = ?`,
		table, column, referencedTable,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	constraints := make([]string, 0, 1)
	for rows.Next() {
		var constraint string
		if err := rows.Scan(&constraint); err != nil {
			return err
		}
		constraints = append(constraints, constraint)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, constraint := range constraints {
		if _, err := db.Exec(
			"ALTER TABLE " + quoteMySQLIdentifier(table) +
				" DROP FOREIGN KEY " + quoteMySQLIdentifier(constraint),
		); err != nil {
			return err
		}
	}
	return nil
}

func ensureAssetOwnerSurvivesUserDeletion(db *sql.DB) error {
	var nullable string
	if err := db.QueryRow(
		`SELECT IS_NULLABLE
		 FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'assets' AND COLUMN_NAME = 'owner_user_id'`,
	).Scan(&nullable); err != nil {
		return err
	}

	var totalConstraints, setNullConstraints int
	if err := db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN r.DELETE_RULE = 'SET NULL' THEN 1 ELSE 0 END), 0)
		 FROM information_schema.KEY_COLUMN_USAGE k
		 JOIN information_schema.REFERENTIAL_CONSTRAINTS r
		   ON r.CONSTRAINT_SCHEMA = k.CONSTRAINT_SCHEMA
		  AND r.TABLE_NAME = k.TABLE_NAME
		  AND r.CONSTRAINT_NAME = k.CONSTRAINT_NAME
		 WHERE k.TABLE_SCHEMA = DATABASE()
		   AND k.TABLE_NAME = 'assets'
		   AND k.COLUMN_NAME = 'owner_user_id'
		   AND k.REFERENCED_TABLE_NAME = 'users'`,
	).Scan(&totalConstraints, &setNullConstraints); err != nil {
		return err
	}
	if strings.EqualFold(nullable, "YES") && totalConstraints > 0 && totalConstraints == setNullConstraints {
		return nil
	}

	if err := dropForeignKeysForColumn(db, "assets", "owner_user_id", "users"); err != nil {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE assets MODIFY COLUMN owner_user_id VARCHAR(128) NULL`); err != nil {
		return err
	}
	_, err := db.Exec(
		`ALTER TABLE assets
		 ADD CONSTRAINT fk_assets_owner
		 FOREIGN KEY (owner_user_id) REFERENCES users(id) ON DELETE SET NULL`,
	)
	return err
}

func quoteMySQLIdentifier(value string) string {
	return "`" + strings.ReplaceAll(value, "`", "``") + "`"
}
