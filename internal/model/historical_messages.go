package model

import (
	"database/sql"
	"strings"
)

// SQLExecer is implemented by both *sql.DB and *sql.Tx.
type SQLExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
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
		`UPDATE messages
		 SET sender_uid_snapshot = src.uid,
		     sender_username_snapshot = src.username,
		     sender_display_name_snapshot = src.display_name,
		     sender_room_display_name_snapshot = src.room_display_name,
		     sender_avatar_url_snapshot = src.avatar_url,
		     sender_default_avatar_key_snapshot = src.default_avatar_key,
		     sender_is_superuser_snapshot = src.is_superuser,
		     sender_room_role_snapshot = src.room_role
		 FROM (
		   SELECT m.id AS message_id,
		          COALESCE(u.uid, '') AS uid,
		          u.username,
		          u.display_name,
		          rm.room_display_name,
		          u.avatar_url,
		          COALESCE(NULLIF(u.default_avatar_key, ''), 'blue-3') AS default_avatar_key,
		          u.is_superuser,
		          CASE
		            WHEN u.is_superuser != 0 THEN 'superuser'
		            ELSE COALESCE(rm.role, '')
		          END AS room_role
		   FROM messages m
		   JOIN users u ON u.id = m.sender_user_id
		   LEFT JOIN room_memberships rm
		     ON rm.room_id = m.room_id AND rm.user_id = m.sender_user_id
		   WHERE `+where+`
		 ) AS src
		 WHERE messages.id = src.message_id`,
		args...,
	)
	return err
}
