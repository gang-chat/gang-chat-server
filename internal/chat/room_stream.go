package chat

import (
	"database/sql"
	"errors"

	"github.com/zhuangkaiyi/gang-chat/server/internal/eventbus"
)

// Room-list realtime sync. Membership and room metadata are low-frequency
// relative to live audio, so rather than diffing we rebuild a full snapshot
// on every change and let clients blindly overwrite their list entry. Three
// events cover the lifecycle:
//
//   - room_added   full snapshot, sent to a user who just gained membership
//   - room_updated full snapshot, sent to every current member
//   - room_deleted just {room_id}, sent to users who lost the room
//
// Delivery is per-user (PublishUser), not per-room (PublishRoom). The room
// interest set on an SSE connection is fixed at connect time, so a freshly
// joined member's existing connection isn't subscribed to the new room and a
// PublishRoom broadcast would miss them. A connection's userID, by contrast,
// never changes, so addressing the audience by userID reaches every online
// member — including the new one — regardless of their interest set. Offline
// members simply have no connections and heal on reconnect (liveStream
// re-derives their rooms from the DB), so we never persist any of this.
//
// The base snapshot is user-agnostic. Just before delivery we copy it and add
// per-recipient fields such as unread_count, notification_policy, is_pinned and
// remark_name.

// roomSnapshot is the shared, user-agnostic view of a room pushed over SSE.
// It mirrors roomCard minus the per-viewer fields.
type roomSnapshot struct {
	ID                          string              `json:"id"`
	RID                         string              `json:"rid,omitempty"`
	Name                        string              `json:"name"`
	RemarkName                  *string             `json:"remark_name,omitempty"`
	Description                 string              `json:"description"`
	AvatarURL                   *string             `json:"avatar_url"`
	DefaultAvatarKey            string              `json:"default_avatar_key"`
	Visibility                  string              `json:"visibility,omitempty"`
	JoinPolicy                  string              `json:"join_policy,omitempty"`
	AIVoiceAnnounceEnabled      bool                `json:"ai_voice_announce_enabled"`
	AIVoiceAnnouncementsEnabled bool                `json:"ai_voice_announcements_enabled"`
	MessageRecallPolicy         string              `json:"message_recall_policy,omitempty"`
	MessageRecallWindowSeconds  *int64              `json:"message_recall_window_seconds"`
	NotificationPolicy          string              `json:"notification_policy,omitempty"`
	IsPinned                    bool                `json:"is_pinned"`
	MemberCount                 int                 `json:"member_count"`
	OnlineMemberCount           int                 `json:"online_member_count"`
	LiveParticipantCount        int                 `json:"live_participant_count"`
	LiveAvatarPreview           []userSummary       `json:"live_avatar_preview"`
	LastMessage                 *lastMessagePreview `json:"last_message"`
	UnreadCount                 int                 `json:"unread_count"`
	CreatedAt                   string              `json:"created_at"`
	UpdatedAt                   string              `json:"updated_at"`
}

// buildRoomSnapshot rebuilds the full public snapshot for roomID from the DB.
func (h *Handler) buildRoomSnapshot(roomID string) (roomSnapshot, error) {
	var rec roomRecord
	err := h.DB.QueryRow(
		`SELECT r.id, r.rid, r.name, r.avatar_url, r.default_avatar_key, r.created_by_user_id,
		        r.visibility, r.join_policy, r.ai_voice_announce_enabled,
		        r.message_recall_policy, r.message_recall_window_seconds,
		        r.description, r.created_at, r.updated_at
		 FROM rooms r WHERE r.id = ?`,
		roomID,
	).Scan(
		&rec.ID, &rec.RID, &rec.Name, &rec.AvatarURL, &rec.DefaultAvatarKey, &rec.CreatedByUserID,
		&rec.Visibility, &rec.JoinPolicy, &rec.AIVoiceAnnounceEnabled, &rec.MessageRecallPolicy,
		&rec.MessageRecallWindowSeconds, &rec.Description, &rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		return roomSnapshot{}, err
	}

	memberCount, err := h.memberCount(roomID)
	if err != nil {
		return roomSnapshot{}, err
	}
	onlineMemberCount, err := h.onlineMemberCount(roomID)
	if err != nil {
		return roomSnapshot{}, err
	}
	livePreview, liveCount, err := h.livePreview(roomID)
	if err != nil {
		return roomSnapshot{}, err
	}
	lastMessage, err := h.lastMessage(roomID)
	if err != nil {
		return roomSnapshot{}, err
	}

	return roomSnapshot{
		ID:                          rec.ID,
		RID:                         rec.RID.String,
		Name:                        rec.Name,
		Description:                 rec.Description,
		AvatarURL:                   nullableString(rec.AvatarURL),
		DefaultAvatarKey:            rec.DefaultAvatarKey,
		Visibility:                  rec.Visibility,
		JoinPolicy:                  rec.JoinPolicy,
		AIVoiceAnnounceEnabled:      rec.AIVoiceAnnounceEnabled != 0,
		AIVoiceAnnouncementsEnabled: rec.AIVoiceAnnounceEnabled != 0,
		MessageRecallPolicy:         rec.MessageRecallPolicy,
		MessageRecallWindowSeconds:  nullableInt64(rec.MessageRecallWindowSeconds),
		MemberCount:                 memberCount,
		OnlineMemberCount:           onlineMemberCount,
		LiveParticipantCount:        liveCount,
		LiveAvatarPreview:           livePreview,
		LastMessage:                 lastMessage,
		CreatedAt:                   formatMillis(rec.CreatedAt),
		UpdatedAt:                   formatMillis(rec.UpdatedAt),
	}, nil
}

// roomMemberIDs returns every user_id that currently belongs to roomID. This
// is the audience for room_updated: we address members by id and let the bus
// drop the ones with no live connection.
func (h *Handler) roomMemberIDs(roomID string) ([]string, error) {
	rows, err := h.DB.Query(`SELECT user_id FROM room_memberships WHERE room_id = ?`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// publishRoomToUser sends a full room snapshot to a single user under
// eventType (room_added or room_updated). Best-effort: a nil bus, a missing
// room (already deleted), or any build error is swallowed so it never blocks
// the HTTP write path that triggered it.
func (h *Handler) publishRoomToUser(userID, roomID, eventType string) {
	if h == nil || h.Bus == nil || userID == "" || roomID == "" {
		return
	}
	snapshot, err := h.buildRoomSnapshot(roomID)
	if err != nil {
		return
	}
	h.applyRoomSnapshotPersonalFields(&snapshot, roomID, userID)
	h.Bus.PublishUser(userID, eventbus.Event{
		Type:   eventType,
		RoomID: roomID,
		Data:   snapshot,
	})
}

func (h *Handler) applyRoomSnapshotPersonalFields(snapshot *roomSnapshot, roomID, userID string) {
	if snapshot == nil || roomID == "" || userID == "" {
		return
	}
	var remarkName sql.NullString
	var notificationPolicy string
	var isPinned int
	if err := h.DB.QueryRow(
		`SELECT remark_name, notification_level, is_pinned
		 FROM room_memberships
		 WHERE room_id = ? AND user_id = ?`,
		roomID, userID,
	).Scan(&remarkName, &notificationPolicy, &isPinned); err != nil {
		return
	}
	snapshot.RemarkName = nullableString(remarkName)
	snapshot.NotificationPolicy = notificationPolicy
	snapshot.IsPinned = isPinned != 0
	snapshot.UnreadCount = h.unreadCount(roomID, userID)
	if notificationPolicy == "blocked" {
		snapshot.LastMessage = nil
		snapshot.UnreadCount = 0
	}
}

// publishRoomUpdated rebuilds the snapshot once and pushes a room_updated to
// every current member of roomID, skipping any userID in exclude (used when a
// member who just got their own room_added shouldn't also get room_updated).
func (h *Handler) publishRoomUpdated(roomID string, exclude ...string) {
	h.publishRoomUpdatedWithOptions(roomID, false, exclude...)
}

func (h *Handler) publishRoomMessageUpdated(roomID string, exclude ...string) {
	h.publishRoomUpdatedWithOptions(roomID, true, exclude...)
}

func (h *Handler) publishRoomUpdatedWithOptions(roomID string, skipBlockedMessages bool, exclude ...string) {
	if h == nil || h.Bus == nil || roomID == "" {
		return
	}
	snapshot, err := h.buildRoomSnapshot(roomID)
	if errors.Is(err, sql.ErrNoRows) {
		return // room is gone; room_deleted covers this case instead
	}
	if err != nil {
		return
	}
	members, err := h.roomMemberIDs(roomID)
	if err != nil {
		return
	}
	skip := make(map[string]struct{}, len(exclude))
	for _, id := range exclude {
		skip[id] = struct{}{}
	}
	for _, userID := range members {
		if _, ok := skip[userID]; ok {
			continue
		}
		if skipBlockedMessages && h.roomMessagesBlocked(roomID, userID) {
			continue
		}
		userSnapshot := snapshot
		h.applyRoomSnapshotPersonalFields(&userSnapshot, roomID, userID)
		ev := eventbus.Event{
			Type:   "room_updated",
			RoomID: roomID,
			Data:   userSnapshot,
		}
		h.Bus.PublishUser(userID, ev)
	}
}

func (h *Handler) publishRoomsUpdated(roomIDs []string) {
	seen := map[string]bool{}
	for _, roomID := range roomIDs {
		if roomID == "" || seen[roomID] {
			continue
		}
		seen[roomID] = true
		h.publishRoomUpdated(roomID)
	}
}

// publishRoomDeleted tells the given users to drop roomID from their list. The
// payload is intentionally minimal — there's nothing left to snapshot. Used
// both when a room is destroyed (audience = its former members) and when a
// single user leaves or is removed (audience = just that user).
func (h *Handler) publishRoomDeleted(roomID string, userIDs ...string) {
	if h == nil || h.Bus == nil || roomID == "" {
		return
	}
	ev := eventbus.Event{
		Type:   "room_deleted",
		RoomID: roomID,
		Data:   map[string]any{"room_id": roomID},
	}
	for _, userID := range userIDs {
		if userID == "" {
			continue
		}
		h.Bus.PublishUser(userID, ev)
	}
}

func (h *Handler) publishRoomInvitesUpdated(userID string) {
	if h == nil || h.Bus == nil || userID == "" {
		return
	}
	h.Bus.PublishUser(userID, eventbus.Event{
		Type: "room_invites_updated",
		Data: map[string]any{"has_changes": true},
	})
}

func (h *Handler) publishRoomInvitesUpdatedForUsers(userIDs ...string) {
	seen := map[string]bool{}
	for _, userID := range userIDs {
		if userID == "" || seen[userID] {
			continue
		}
		seen[userID] = true
		h.publishRoomInvitesUpdated(userID)
	}
}

func (h *Handler) pendingRoomInviteTargetIDs(roomID string) []string {
	if h == nil || h.DB == nil || roomID == "" {
		return nil
	}
	rows, err := h.DB.Query(
		`SELECT DISTINCT target_user_id
		 FROM room_invites
		 WHERE room_id = ? AND status = 'pending'`,
		roomID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	userIDs := make([]string, 0)
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err == nil {
			userIDs = append(userIDs, userID)
		}
	}
	return userIDs
}

func (h *Handler) publishPendingRoomInvitesUpdatedForInviter(roomID, inviterID string) {
	if h == nil || h.DB == nil || roomID == "" || inviterID == "" {
		return
	}
	rows, err := h.DB.Query(
		`SELECT target_user_id
		 FROM room_invites
		 WHERE room_id = ? AND inviter_user_id = ? AND status = 'pending'`,
		roomID, inviterID,
	)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err == nil {
			h.publishRoomInvitesUpdated(userID)
		}
	}
}

func (h *Handler) publishPendingRoomInvitesUpdatedForRoom(roomID string) {
	h.publishRoomInvitesUpdatedForUsers(h.pendingRoomInviteTargetIDs(roomID)...)
}

func (h *Handler) publishRoomApplicationsUpdated(userID string) {
	if h == nil || h.Bus == nil || userID == "" {
		return
	}
	h.Bus.PublishUser(userID, eventbus.Event{
		Type: "room_applications_updated",
		Data: map[string]any{"has_changes": true},
	})
}

func (h *Handler) publishRoomNotificationsUpdated(userID string) {
	if h == nil || h.Bus == nil || userID == "" {
		return
	}
	h.Bus.PublishUser(userID, eventbus.Event{
		Type: "room_notifications_updated",
		Data: map[string]any{"has_changes": true},
	})
}

func (h *Handler) publishRoomJoinRequestsUpdated(roomID string) {
	if h == nil || h.Bus == nil || roomID == "" {
		return
	}
	members, err := h.roomMemberIDs(roomID)
	if err != nil {
		return
	}
	ev := eventbus.Event{
		Type:   "room_join_requests_updated",
		RoomID: roomID,
		Data:   map[string]any{"room_id": roomID},
	}
	for _, userID := range members {
		h.Bus.PublishUser(userID, ev)
	}
}

// publishRoomRole tells a single user their role in roomID changed. Role is a
// per-viewer field (my_role) that the shared snapshot deliberately omits, so a
// role change has no other way to reach the affected member. The payload reads
// the freshly committed role straight from the DB.
func (h *Handler) publishRoomRole(roomID, userID string) {
	if h == nil || h.Bus == nil || roomID == "" || userID == "" {
		return
	}
	var role string
	if err := h.DB.QueryRow(
		`SELECT role FROM room_memberships WHERE room_id = ? AND user_id = ?`,
		roomID, userID,
	).Scan(&role); err != nil {
		return
	}
	h.Bus.PublishUser(userID, eventbus.Event{
		Type:   "room_role_changed",
		RoomID: roomID,
		Data:   map[string]any{"room_id": roomID, "role": role},
	})
}
