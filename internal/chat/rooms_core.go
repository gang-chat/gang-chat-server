package chat

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/idgen"
)

func (h *Handler) listRooms(c *gin.Context) {
	userID := currentUserID(c)
	limit := parseLimit(c.Query("limit"), 50, 100)

	var rows *sql.Rows
	var err error
	if h.isSuperuser(userID) {
		rows, err = h.DB.Query(
			`SELECT r.id, r.rid, r.name, r.avatar_url, r.default_avatar_key, r.created_by_user_id,
			        r.visibility, r.join_policy, r.ai_voice_announce_enabled,
			        r.message_recall_policy, r.message_recall_window_seconds,
			        r.description, r.created_at, r.updated_at
			 FROM rooms r
			 ORDER BY (
			   SELECT COUNT(*) FROM live_participants lp WHERE lp.room_id = r.id
			 ) = 0, r.updated_at DESC, r.created_at DESC
			 LIMIT ?`,
			limit,
		)
	} else {
		rows, err = h.DB.Query(
			`SELECT r.id, r.rid, r.name, r.avatar_url, r.default_avatar_key, r.created_by_user_id,
			        r.visibility, r.join_policy, r.ai_voice_announce_enabled,
			        r.message_recall_policy, r.message_recall_window_seconds,
			        r.description, r.created_at, r.updated_at
			 FROM rooms r
			 JOIN room_memberships rm ON rm.room_id = r.id
			 WHERE rm.user_id = ?
			 ORDER BY (
			   SELECT COUNT(*) FROM live_participants lp WHERE lp.room_id = r.id
			 ) = 0, r.updated_at DESC, rm.joined_at DESC
			 LIMIT ?`,
			userID, limit,
		)
	}
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to list rooms")
		return
	}
	defer rows.Close()

	rooms := make([]roomCard, 0)
	for rows.Next() {
		rec, err := scanRoomRecord(rows)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read rooms")
			return
		}
		card, err := h.buildRoomCard(rec, userID)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to build room card")
			return
		}
		rooms = append(rooms, card)
	}

	c.JSON(http.StatusOK, gin.H{"rooms": rooms, "next_cursor": nil})
}

func (h *Handler) createRoom(c *gin.Context) {
	var req createRoomRequest
	rawBody, ok := h.bindJSON(c, &req)
	if !ok {
		return
	}
	if h.replayIdempotency(c, rawBody) {
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" || utf8.RuneCountInString(name) > 50 {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "Room name is required and must be at most 50 characters.")
		return
	}

	now := nowMillis()
	roomID := newID("room")
	rid := idgen.NextRoomRID(h.DB)
	userID := currentUserID(c)
	visibility := req.Visibility
	if visibility == "" {
		visibility = "public"
	}
	if !allowed(visibility, "public", "private") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid visibility")
		return
	}
	joinPolicy := req.JoinPolicy
	if joinPolicy == "" {
		joinPolicy = "approval_required"
	}
	if !allowed(joinPolicy, "open", "approval_required", "closed") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid join_policy")
		return
	}
	description := strings.TrimSpace(req.Description)
	if utf8.RuneCountInString(description) > 500 {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "description must be at most 500 characters")
		return
	}
	defaultAvatarKey := defaultRoomAvatar(roomID)
	if req.DefaultAvatarKey != nil && strings.TrimSpace(*req.DefaultAvatarKey) != "" {
		defaultAvatarKey = strings.TrimSpace(*req.DefaultAvatarKey)
	}
	var avatarAssetID any
	var avatarURL any
	if req.AvatarAssetID != nil && strings.TrimSpace(*req.AvatarAssetID) != "" {
		var url string
		if err := h.DB.QueryRow(`SELECT url FROM assets WHERE id = ? AND owner_user_id = ?`, strings.TrimSpace(*req.AvatarAssetID), userID).Scan(&url); err != nil {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "avatar asset not found")
			return
		}
		avatarAssetID = strings.TrimSpace(*req.AvatarAssetID)
		avatarURL = url
	}

	tx, err := h.DB.Begin()
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to create room")
		return
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO rooms (
		   id, rid, name, avatar_asset_id, avatar_url, default_avatar_key, created_by_user_id,
		   visibility, join_policy, ai_voice_announce_enabled, message_recall_policy,
		   message_recall_window_seconds, description, created_at, updated_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 'time_limited', 120, ?, ?, ?)`,
		roomID, rid, name, avatarAssetID, avatarURL, defaultAvatarKey, userID, visibility, joinPolicy, description, now, now,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to create room")
		return
	}
	if !h.isSuperuser(userID) {
		_, err = tx.Exec(
			`INSERT INTO room_memberships (room_id, user_id, role, joined_at) VALUES (?, ?, 'owner', ?)`,
			roomID, userID, now,
		)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to join room")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to save room")
		return
	}

	detail, err := h.buildRoomDetail(roomID, userID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room")
		return
	}
	h.publishRoomToUser(userID, roomID, "room_added")
	h.idempotentJSON(c, http.StatusCreated, rawBody, gin.H{"room": detail})
}

func (h *Handler) searchRooms(c *gin.Context) {
	query := strings.TrimSpace(c.Query("q"))
	if query == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "room search keyword is required")
		return
	}

	userID := currentUserID(c)
	limit := parseLimit(c.Query("limit"), 20, 50)
	superuser := h.isSuperuser(userID)
	visibilityFilter := `(r.rid = ? OR (r.visibility = 'public' AND instr(lower(r.name), lower(?)) > 0))`
	if superuser {
		visibilityFilter = `(r.rid = ? OR instr(lower(r.name), lower(?)) > 0)`
	}
	rows, err := h.DB.Query(
		`SELECT r.id, r.rid, r.name, r.avatar_url, r.default_avatar_key, r.created_by_user_id,
		        r.visibility, r.join_policy, r.ai_voice_announce_enabled,
		        r.message_recall_policy, r.message_recall_window_seconds,
		        r.description, r.created_at, r.updated_at,
		        CASE WHEN rm.user_id IS NULL THEN 0 ELSE 1 END AS joined
		 FROM rooms r
		 LEFT JOIN room_memberships rm ON rm.room_id = r.id AND rm.user_id = ?
		 WHERE `+visibilityFilter+`
		 ORDER BY joined DESC, r.updated_at DESC, r.name ASC
		 LIMIT ?`,
		userID, query, query, limit,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to search rooms")
		return
	}
	defer rows.Close()

	rooms := make([]publicRoom, 0)
	for rows.Next() {
		rec, joined, err := scanPublicRoomRecord(rows)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room search results")
			return
		}
		memberCount, err := h.memberCount(rec.ID)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room members")
			return
		}
		onlineMemberCount, err := h.onlineMemberCount(rec.ID)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read online members")
			return
		}
		_, liveCount, err := h.livePreview(rec.ID)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read live state")
			return
		}
		rooms = append(rooms, publicRoom{
			ID:                   rec.ID,
			RID:                  rec.RID.String,
			Name:                 rec.Name,
			AvatarURL:            nullableString(rec.AvatarURL),
			DefaultAvatarKey:     rec.DefaultAvatarKey,
			Visibility:           rec.Visibility,
			JoinPolicy:           rec.JoinPolicy,
			MemberCount:          memberCount,
			OnlineMemberCount:    onlineMemberCount,
			LiveParticipantCount: liveCount,
			Joined:               joined,
			JoinState:            h.joinState(rec.ID, userID, joined),
			UpdatedAt:            formatMillis(rec.UpdatedAt),
		})
	}

	c.JSON(http.StatusOK, gin.H{"rooms": rooms, "next_cursor": nil})
}

func (h *Handler) getRoom(c *gin.Context) {
	detail, err := h.buildRoomDetail(c.Param("room_id"), currentUserID(c))
	if errors.Is(err, sql.ErrNoRows) {
		h.jsonError(c, http.StatusNotFound, "not_found", "room not found")
		return
	}
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read room")
		return
	}
	c.JSON(http.StatusOK, gin.H{"room": detail})
}

func (h *Handler) buildRoomCard(rec roomRecord, userID string) (roomCard, error) {
	memberCount, err := h.memberCount(rec.ID)
	if err != nil {
		return roomCard{}, err
	}
	onlineMemberCount, err := h.onlineMemberCount(rec.ID)
	if err != nil {
		return roomCard{}, err
	}
	livePreview, liveCount, err := h.livePreview(rec.ID)
	if err != nil {
		return roomCard{}, err
	}
	lastMessage, err := h.lastMessage(rec.ID)
	if err != nil {
		return roomCard{}, err
	}
	var role, notificationLevel string
	var remarkName sql.NullString
	_ = h.DB.QueryRow(
		`SELECT role, notification_level, remark_name FROM room_memberships WHERE room_id = ? AND user_id = ?`,
		rec.ID, userID,
	).Scan(&role, &notificationLevel, &remarkName)
	if role == "" && h.isSuperuser(userID) {
		role = "superuser"
		notificationLevel = "all"
	}
	return roomCard{
		ID:                   rec.ID,
		RID:                  rec.RID.String,
		Name:                 rec.Name,
		RemarkName:           nullableString(remarkName),
		Description:          rec.Description,
		Visibility:           rec.Visibility,
		JoinPolicy:           rec.JoinPolicy,
		AvatarURL:            nullableString(rec.AvatarURL),
		DefaultAvatarKey:     rec.DefaultAvatarKey,
		MemberCount:          memberCount,
		MyRole:               role,
		NotificationLevel:    notificationLevel,
		NotificationPolicy:   notificationLevel,
		OnlineMemberCount:    onlineMemberCount,
		LiveParticipantCount: liveCount,
		LiveAvatarPreview:    livePreview,
		LastMessage:          lastMessage,
		UnreadCount:          h.unreadCount(rec.ID, userID),
		UpdatedAt:            formatMillis(rec.UpdatedAt),
	}, nil
}

func (h *Handler) buildRoomDetail(roomID, userID string) (roomDetail, error) {
	var rec roomRecord
	var joinedAt int64
	var role, notificationLevel string
	var remarkName, roomDisplayName, roomAvatarURL, roomDefaultAvatarKey sql.NullString
	err := h.DB.QueryRow(
		`SELECT r.id, r.rid, r.name, r.avatar_url, r.default_avatar_key, r.created_by_user_id,
		        r.visibility, r.join_policy, r.ai_voice_announce_enabled,
		        r.message_recall_policy, r.message_recall_window_seconds,
		        r.description, r.created_at, r.updated_at,
		        rm.role, rm.remark_name, rm.room_display_name, rm.room_avatar_url,
		        rm.room_default_avatar_key, rm.notification_level, rm.joined_at
		 FROM rooms r
		 JOIN room_memberships rm ON rm.room_id = r.id
		 WHERE r.id = ? AND rm.user_id = ?`,
		roomID, userID,
	).Scan(
		&rec.ID, &rec.RID, &rec.Name, &rec.AvatarURL, &rec.DefaultAvatarKey, &rec.CreatedByUserID,
		&rec.Visibility, &rec.JoinPolicy, &rec.AIVoiceAnnounceEnabled, &rec.MessageRecallPolicy,
		&rec.MessageRecallWindowSeconds, &rec.Description, &rec.CreatedAt, &rec.UpdatedAt,
		&role, &remarkName, &roomDisplayName, &roomAvatarURL, &roomDefaultAvatarKey, &notificationLevel, &joinedAt,
	)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) || !h.isSuperuser(userID) {
			return roomDetail{}, err
		}
		err = h.DB.QueryRow(
			`SELECT r.id, r.rid, r.name, r.avatar_url, r.default_avatar_key, r.created_by_user_id,
			        r.visibility, r.join_policy, r.ai_voice_announce_enabled,
			        r.message_recall_policy, r.message_recall_window_seconds,
			        r.description, r.created_at, r.updated_at
			 FROM rooms r
			 WHERE r.id = ?`,
			roomID,
		).Scan(
			&rec.ID, &rec.RID, &rec.Name, &rec.AvatarURL, &rec.DefaultAvatarKey, &rec.CreatedByUserID,
			&rec.Visibility, &rec.JoinPolicy, &rec.AIVoiceAnnounceEnabled, &rec.MessageRecallPolicy,
			&rec.MessageRecallWindowSeconds, &rec.Description, &rec.CreatedAt, &rec.UpdatedAt,
		)
		if err != nil {
			return roomDetail{}, err
		}
		role = "superuser"
		notificationLevel = "all"
		joinedAt = rec.CreatedAt
	}

	var createdBy *userSummary
	if rec.CreatedByUserID.Valid && !h.shouldHideRoomCreator(rec) {
		summary, err := h.userSummary(rec.CreatedByUserID.String)
		if err != nil {
			return roomDetail{}, err
		}
		createdBy = &summary
	}
	memberCount, err := h.memberCount(roomID)
	if err != nil {
		return roomDetail{}, err
	}
	onlineMemberCount, err := h.onlineMemberCount(roomID)
	if err != nil {
		return roomDetail{}, err
	}
	live, err := h.buildLiveState(roomID, rec.UpdatedAt)
	if err != nil {
		return roomDetail{}, err
	}

	return roomDetail{
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
		CreatedBy:                   createdBy,
		RemarkName:                  nullableString(remarkName),
		NotificationPolicy:          notificationLevel,
		PersonalProfile: roomPersonalProfile{
			DisplayName:      nullableString(roomDisplayName),
			AvatarURL:        nullableString(roomAvatarURL),
			DefaultAvatarKey: nullableString(roomDefaultAvatarKey),
		},
		CanDeleteRoom: role == "owner" || role == "superuser" || h.isSuperuser(userID),
		MyMembership: roomMembership{
			Role:                 role,
			JoinedAt:             formatMillis(joinedAt),
			RemarkName:           nullableString(remarkName),
			RoomDisplayName:      nullableString(roomDisplayName),
			RoomAvatarURL:        nullableString(roomAvatarURL),
			RoomDefaultAvatarKey: nullableString(roomDefaultAvatarKey),
			NotificationLevel:    notificationLevel,
		},
		Live:      live,
		CreatedAt: formatMillis(rec.CreatedAt),
		UpdatedAt: formatMillis(rec.UpdatedAt),
	}, nil
}

func (h *Handler) shouldHideRoomCreator(rec roomRecord) bool {
	if !rec.CreatedByUserID.Valid || rec.CreatedByUserID.String == "" {
		return true
	}
	if !h.isSuperuser(rec.CreatedByUserID.String) {
		return false
	}
	return !h.roomHasOwner(rec.ID)
}

func (h *Handler) roomHasOwner(roomID string) bool {
	var exists int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ? AND role = 'owner'`, roomID).Scan(&exists)
	return exists != 0
}

func (h *Handler) memberCount(roomID string) (int, error) {
	var count int
	err := h.DB.QueryRow(`SELECT COUNT(*) FROM room_memberships WHERE room_id = ?`, roomID).Scan(&count)
	return count, err
}

func (h *Handler) onlineMemberCount(roomID string) (int, error) {
	if h.Bus == nil {
		return 0, nil
	}
	online := h.Bus.OnlineUserIDs()
	if len(online) == 0 {
		return 0, nil
	}
	rows, err := h.DB.Query(`SELECT user_id FROM room_memberships WHERE room_id = ?`, roomID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return 0, err
		}
		if _, ok := online[userID]; ok {
			count++
		}
	}
	return count, rows.Err()
}

func (h *Handler) livePreview(roomID string) ([]userSummary, int, error) {
	rows, err := h.DB.Query(
		`SELECT u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key
		 FROM live_participants lp
		 JOIN users u ON u.id = lp.user_id
		 WHERE lp.room_id = ?
		 ORDER BY lp.joined_at ASC`,
		roomID,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	preview := make([]userSummary, 0, 5)
	count := 0
	for rows.Next() {
		var id, uid, username string
		var displayName, avatarURL, defaultAvatar sql.NullString
		if err := rows.Scan(&id, &uid, &username, &displayName, &avatarURL, &defaultAvatar); err != nil {
			return nil, 0, err
		}
		if len(preview) < 5 {
			preview = append(preview, summaryFromUserFields(id, uid, username, displayName, avatarURL, defaultAvatar))
		}
		count++
	}
	return preview, count, nil
}

func (h *Handler) lastMessage(roomID string) (*lastMessagePreview, error) {
	var id, sender, body string
	var createdAt int64
	err := h.DB.QueryRow(
		`SELECT m.id, u.username, m.body, m.created_at
		 FROM messages m
		 JOIN users u ON u.id = m.sender_user_id
		 WHERE m.room_id = ?
		 ORDER BY m.created_at DESC
		 LIMIT 1`,
		roomID,
	).Scan(&id, &sender, &body, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &lastMessagePreview{
		ID:                id,
		SenderDisplayName: sender,
		BodyPreview:       preview(body, 80),
		CreatedAt:         formatMillis(createdAt),
	}, nil
}

func (h *Handler) unreadCount(roomID, userID string) int {
	var readAt int64
	_ = h.DB.QueryRow(
		`SELECT m.created_at
		 FROM room_reads rr
		 JOIN messages m ON m.id = rr.last_read_message_id
		 WHERE rr.room_id = ? AND rr.user_id = ?`,
		roomID, userID,
	).Scan(&readAt)

	var count int
	_ = h.DB.QueryRow(
		`SELECT COUNT(*) FROM messages
		 WHERE room_id = ? AND sender_user_id != ? AND created_at > ?`,
		roomID, userID, readAt,
	).Scan(&count)
	return count
}
