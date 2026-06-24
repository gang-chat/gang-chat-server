package chat

import "database/sql"

const (
	systemMessageType            = "system"
	systemEventRoomMemberJoined  = "room_member_joined"
	systemEventRoomMemberLeft    = "room_member_left"
	systemEventRoomMemberRemoved = "room_member_removed"
	systemEventLiveJoined        = "live_joined"
	systemEventLiveLeft          = "live_left"
	systemEventRoomRoleChanged   = "room_role_changed"
)

func visibleMessageSQL(alias string) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	return `NOT (` + prefix + `type = 'system'
		AND EXISTS (
			SELECT 1
			FROM json_each(` + prefix + `attachments_json) attachment
			WHERE lower(COALESCE(json_extract(attachment.value, '$.type'), '')) = 'system'
			  AND lower(COALESCE(json_extract(attachment.value, '$.event'), '')) IN ('live_joined', 'live_left')
		)
	)`
}

type systemMessageSpec struct {
	Event    string
	UserID   string
	ActorID  string
	TargetID string
	FromRole string
	ToRole   string
}

func (h *Handler) appendSystemMessage(roomID string, spec systemMessageSpec) error {
	tx, err := h.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := h.appendSystemMessageTx(tx, roomID, spec); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *Handler) appendSystemMessageTx(tx *sql.Tx, roomID string, spec systemMessageSpec) error {
	subjectID := firstNonEmpty(spec.TargetID, spec.UserID)
	if spec.Event == "" || subjectID == "" {
		return nil
	}
	subject, err := userSummaryByRoomIDTx(tx, roomID, subjectID)
	if err != nil {
		return err
	}

	attachment := map[string]any{
		"type":  systemMessageType,
		"event": spec.Event,
		"user":  subject,
	}
	if spec.TargetID != "" {
		attachment["target"] = subject
	}

	actorName := ""
	if spec.ActorID != "" {
		actor, err := userSummaryByRoomIDTx(tx, roomID, spec.ActorID)
		if err != nil {
			return err
		}
		attachment["actor"] = actor
		actorName = actor.DisplayName
	}
	if spec.FromRole != "" {
		attachment["from_role"] = spec.FromRole
	}
	if spec.ToRole != "" {
		attachment["to_role"] = spec.ToRole
	}

	now := nowMillis()
	_, err = tx.Exec(
		`INSERT INTO messages (id, room_id, sender_user_id, client_message_id, type, body, mentions_json, attachments_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newID("msg"),
		roomID,
		subjectID,
		newID("sys"),
		systemMessageType,
		systemMessageBody(spec, actorName),
		mustJSON(nil),
		mustJSON([]map[string]any{attachment}),
		now,
	)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE rooms SET updated_at = ? WHERE id = ?`, now, roomID)
	return err
}

func userSummaryByRoomIDTx(tx *sql.Tx, roomID, userID string) (userSummary, error) {
	var id, uid, username string
	var displayName, avatarURL, defaultAvatar, roomDisplayName, roomRole sql.NullString
	var isSuperuser int
	err := tx.QueryRow(
		`SELECT u.id, u.uid, u.username, u.display_name, u.avatar_url,
		        u.default_avatar_key, u.is_superuser, rm.room_display_name, rm.role
		 FROM users u
		 LEFT JOIN room_memberships rm ON rm.room_id = ? AND rm.user_id = u.id
		 WHERE u.id = ?`,
		roomID, userID,
	).Scan(&id, &uid, &username, &displayName, &avatarURL, &defaultAvatar, &isSuperuser, &roomDisplayName, &roomRole)
	if err != nil {
		return userSummary{}, err
	}
	summary := summaryFromUserFields(id, uid, username, displayName, avatarURL, defaultAvatar)
	summary.RoomDisplayName = nullableString(roomDisplayName)
	if roomRole.Valid && roomRole.String != "" {
		summary.RoomRole = roomRole.String
	} else if isSuperuser != 0 {
		summary.RoomRole = "superuser"
	}
	summary.IsSuperuser = isSuperuser != 0
	return summary, nil
}

func systemMessageBody(spec systemMessageSpec, actorName string) string {
	switch spec.Event {
	case systemEventRoomMemberJoined:
		return "加入了房间"
	case systemEventRoomMemberLeft:
		return "离开了房间"
	case systemEventRoomMemberRemoved:
		if actorName == "" {
			return "被踢出了房间"
		}
		return "被 " + actorName + " 踢出了房间"
	case systemEventLiveJoined:
		return "进入了语音频道"
	case systemEventLiveLeft:
		return "退出了语音频道"
	case systemEventRoomRoleChanged:
		if spec.FromRole == "owner" && spec.ToRole == "admin" {
			return "降职为管理员"
		}
		body := systemRoleChangeVerb(spec.FromRole, spec.ToRole) + systemRoleLabel(spec.ToRole)
		if actorName == "" {
			return body
		}
		return "被 " + actorName + " " + body
	default:
		return ""
	}
}

func systemRoleLabel(role string) string {
	switch role {
	case "owner", "creator":
		return "创建者"
	case "admin":
		return "管理员"
	case "member":
		return "成员"
	default:
		return "成员"
	}
}

func systemRoleChangeVerb(fromRole, toRole string) string {
	if roleRank(toRole) > roleRank(fromRole) {
		return "晋升为"
	}
	return "降职为"
}

func roleRank(role string) int {
	switch role {
	case "owner", "creator":
		return 3
	case "admin":
		return 2
	case "member":
		return 1
	default:
		return 0
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
