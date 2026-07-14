package chat

import "database/sql"

// messageSelectColumnsSQL is shared by every message read path. Display fields
// deliberately come from the immutable send-time snapshot; the live users join
// is used only to mark whether opening the sender card is still meaningful.
const messageSelectColumnsSQL = `m.id, m.room_id, m.client_message_id, m.type, m.body,
	m.mentions_json, m.attachments_json, m.is_recalled, m.recalled_at,
	m.recalled_by_user_id, m.is_force_deleted, m.force_deleted_at,
	m.force_deleted_by_user_id, m.created_at,
	m.sender_user_id,
	COALESCE(NULLIF(m.sender_uid_snapshot, ''), u.uid, ''),
	COALESCE(NULLIF(m.sender_username_snapshot, ''), u.username, ''),
	CASE WHEN m.sender_username_snapshot != '' THEN m.sender_display_name_snapshot ELSE u.display_name END,
	CASE WHEN m.sender_username_snapshot != '' THEN m.sender_avatar_url_snapshot ELSE u.avatar_url END,
	CASE
	  WHEN m.sender_username_snapshot != '' THEN m.sender_default_avatar_key_snapshot
	  ELSE COALESCE(NULLIF(u.default_avatar_key, ''), 'blue-3')
	END,
	CASE WHEN m.sender_username_snapshot != '' THEN m.sender_is_superuser_snapshot ELSE COALESCE(u.is_superuser, 0) END,
	CASE WHEN m.sender_username_snapshot != '' THEN m.sender_room_display_name_snapshot ELSE sender_rm.room_display_name END,
	CASE
	  WHEN m.sender_username_snapshot != '' THEN m.sender_room_role_snapshot
	  WHEN COALESCE(u.is_superuser, 0) != 0 THEN 'superuser'
	  ELSE COALESCE(sender_rm.role, '')
	END,
	CASE WHEN u.id IS NULL THEN 1 ELSE 0 END`

type messageSnapshotExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertMessageWithSenderSnapshot(
	execer messageSnapshotExecer,
	messageID, roomID, senderUserID, clientMessageID, messageType, body,
	mentionsJSON, attachmentsJSON string,
	createdAt int64,
) (sql.Result, error) {
	return execer.Exec(
		`INSERT INTO messages (
		   id, room_id, sender_user_id, client_message_id, type, body,
		   mentions_json, attachments_json, created_at,
		   sender_uid_snapshot, sender_username_snapshot, sender_display_name_snapshot,
		   sender_room_display_name_snapshot, sender_avatar_url_snapshot,
		   sender_default_avatar_key_snapshot, sender_is_superuser_snapshot,
		   sender_room_role_snapshot
		 )
		 SELECT ?, ?, u.id, ?, ?, ?, ?, ?, ?,
		        COALESCE(u.uid, ''), u.username, u.display_name,
		        rm.room_display_name, u.avatar_url,
		        COALESCE(NULLIF(u.default_avatar_key, ''), 'blue-3'),
		        u.is_superuser,
		        CASE WHEN u.is_superuser != 0 THEN 'superuser' ELSE COALESCE(rm.role, '') END
		 FROM users u
		 LEFT JOIN room_memberships rm ON rm.room_id = ? AND rm.user_id = u.id
		 WHERE u.id = ?`,
		messageID,
		roomID,
		clientMessageID,
		messageType,
		body,
		mentionsJSON,
		attachmentsJSON,
		createdAt,
		roomID,
		senderUserID,
	)
}
