package chat

import (
	"database/sql"
	"strings"
)

const (
	systemMessageType                = "system"
	systemEventRoomMemberJoined      = "room_member_joined"
	systemEventRoomMemberLeft        = "room_member_left"
	systemEventRoomMemberRemoved     = "room_member_removed"
	systemEventLiveJoined            = "live_joined"
	systemEventLiveLeft              = "live_left"
	systemEventRoomRoleChanged       = "room_role_changed"
	systemEventRoomNameChanged       = "room_name_changed"
	systemEventRoomBioChanged        = "room_description_changed"
	systemEventRoomVisibilityChanged = "room_visibility_changed"
	systemEventRoomJoinPolicyChanged = "room_join_policy_changed"
)

func visibleMessageSQL(alias string) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	return `NOT (` + prefix + `type = 'system'
		AND EXISTS (
			SELECT 1
			FROM JSON_TABLE(` + prefix + `attachments_json, '$[*]' COLUMNS (
				attachment_type VARCHAR(64) PATH '$.type' NULL ON EMPTY,
				attachment_event VARCHAR(64) PATH '$.event' NULL ON EMPTY
			)) attachment
			WHERE lower(COALESCE(attachment.attachment_type, '')) = 'system'
			  AND lower(COALESCE(attachment.attachment_event, '')) IN ('live_joined', 'live_left')
		)
	)`
}

type systemMessageSpec struct {
	Event     string
	UserID    string
	ActorID   string
	TargetID  string
	FromRole  string
	ToRole    string
	OldValue  string
	NewValue  string
	CreatedAt int64
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
	_, err := h.appendSystemMessageTxWithID(tx, roomID, spec)
	return err
}

func (h *Handler) appendSystemMessageTxWithID(tx *sql.Tx, roomID string, spec systemMessageSpec) (string, error) {
	subjectID := firstNonEmpty(spec.TargetID, spec.UserID)
	if spec.Event == "" || subjectID == "" {
		return "", nil
	}
	subject, err := userSummaryByRoomIDTx(tx, roomID, subjectID)
	if err != nil {
		return "", err
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
			return "", err
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
	if spec.OldValue != "" || systemMessageHasValuePatch(spec.Event) {
		attachment["old_value"] = spec.OldValue
	}
	if spec.NewValue != "" || systemMessageHasValuePatch(spec.Event) {
		attachment["new_value"] = spec.NewValue
	}

	now := spec.CreatedAt
	if now <= 0 {
		now = nowMillis()
	}
	messageID := newID("msg")
	clientMessageID := newID("sys")
	_, err = tx.Exec(
		`INSERT INTO messages (id, room_id, sender_user_id, client_message_id, type, body, mentions_json, attachments_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		messageID,
		roomID,
		subjectID,
		clientMessageID,
		systemMessageType,
		systemMessageBody(spec, actorName),
		mustJSON(nil),
		mustJSON([]map[string]any{attachment}),
		now,
	)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(`UPDATE rooms SET updated_at = ? WHERE id = ?`, now, roomID)
	if err != nil {
		return "", err
	}
	return messageID, nil
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
	case systemEventRoomNameChanged:
		return "房间名称修改为" + spec.NewValue
	case systemEventRoomBioChanged:
		return "房间简介修改为\n" + spec.NewValue
	case systemEventRoomVisibilityChanged:
		return "房间可见性修改为" + systemVisibilityLabel(spec.NewValue)
	case systemEventRoomJoinPolicyChanged:
		return "房间加入方式修改为" + systemJoinPolicyLabel(spec.NewValue)
	default:
		return ""
	}
}

func systemMessageHasValuePatch(event string) bool {
	return event == systemEventRoomNameChanged ||
		event == systemEventRoomBioChanged ||
		event == systemEventRoomVisibilityChanged ||
		event == systemEventRoomJoinPolicyChanged
}

func systemVisibilityLabel(visibility string) string {
	switch strings.ToLower(strings.TrimSpace(visibility)) {
	case "private":
		return "私密"
	default:
		return "公开"
	}
}

func systemJoinPolicyLabel(joinPolicy string) string {
	switch strings.ToLower(strings.TrimSpace(joinPolicy)) {
	case "approval_required":
		return "需审批"
	case "closed":
		return "关闭"
	default:
		return "开放"
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
