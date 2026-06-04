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
// The snapshot is user-agnostic: personal fields (my_role, unread_count,
// remark_name) are deliberately excluded because one payload fans out to many
// users. Clients maintain those locally — e.g. last_message is included so a
// client can bump its own unread counter, but the count itself is never on
// the wire.

// roomSnapshot is the shared, user-agnostic view of a room pushed over SSE.
// It mirrors roomCard minus the per-viewer fields.
type roomSnapshot struct {
	ID                         string              `json:"id"`
	RID                        string              `json:"rid,omitempty"`
	Name                       string              `json:"name"`
	AvatarURL                  *string             `json:"avatar_url"`
	DefaultAvatarKey           string              `json:"default_avatar_key"`
	Visibility                 string              `json:"visibility,omitempty"`
	JoinPolicy                 string              `json:"join_policy,omitempty"`
	AIVoiceAnnounceEnabled     bool                `json:"ai_voice_announce_enabled"`
	MessageRecallPolicy        string              `json:"message_recall_policy,omitempty"`
	MessageRecallWindowSeconds *int64              `json:"message_recall_window_seconds"`
	MemberCount                int                 `json:"member_count"`
	LiveParticipantCount       int                 `json:"live_participant_count"`
	LiveAvatarPreview          []userSummary       `json:"live_avatar_preview"`
	LastMessage                *lastMessagePreview `json:"last_message"`
	CreatedAt                  string              `json:"created_at"`
	UpdatedAt                  string              `json:"updated_at"`
}

// buildRoomSnapshot rebuilds the full public snapshot for roomID from the DB.
func (h *Handler) buildRoomSnapshot(roomID string) (roomSnapshot, error) {
	var rec roomRecord
	err := h.DB.QueryRow(
		`SELECT r.id, r.rid, r.name, r.avatar_url, r.default_avatar_key, r.created_by_user_id,
		        r.visibility, r.join_policy, r.ai_voice_announce_enabled,
		        r.message_recall_policy, r.message_recall_window_seconds,
		        r.created_at, r.updated_at
		 FROM rooms r WHERE r.id = ?`,
		roomID,
	).Scan(
		&rec.ID, &rec.RID, &rec.Name, &rec.AvatarURL, &rec.DefaultAvatarKey, &rec.CreatedByUserID,
		&rec.Visibility, &rec.JoinPolicy, &rec.AIVoiceAnnounceEnabled, &rec.MessageRecallPolicy,
		&rec.MessageRecallWindowSeconds, &rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		return roomSnapshot{}, err
	}

	memberCount, err := h.memberCount(roomID)
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
		ID:                         rec.ID,
		RID:                        rec.RID.String,
		Name:                       rec.Name,
		AvatarURL:                  nullableString(rec.AvatarURL),
		DefaultAvatarKey:           rec.DefaultAvatarKey,
		Visibility:                 rec.Visibility,
		JoinPolicy:                 rec.JoinPolicy,
		AIVoiceAnnounceEnabled:     rec.AIVoiceAnnounceEnabled != 0,
		MessageRecallPolicy:        rec.MessageRecallPolicy,
		MessageRecallWindowSeconds: nullableInt64(rec.MessageRecallWindowSeconds),
		MemberCount:                memberCount,
		LiveParticipantCount:       liveCount,
		LiveAvatarPreview:          livePreview,
		LastMessage:                lastMessage,
		CreatedAt:                  formatMillis(rec.CreatedAt),
		UpdatedAt:                  formatMillis(rec.UpdatedAt),
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
	h.Bus.PublishUser(userID, eventbus.Event{
		Type:   eventType,
		RoomID: roomID,
		Data:   snapshot,
	})
}

// publishRoomUpdated rebuilds the snapshot once and pushes a room_updated to
// every current member of roomID, skipping any userID in exclude (used when a
// member who just got their own room_added shouldn't also get room_updated).
func (h *Handler) publishRoomUpdated(roomID string, exclude ...string) {
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
	ev := eventbus.Event{Type: "room_updated", RoomID: roomID, Data: snapshot}
	for _, userID := range members {
		if _, ok := skip[userID]; ok {
			continue
		}
		h.Bus.PublishUser(userID, ev)
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
