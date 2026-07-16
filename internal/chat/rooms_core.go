package chat

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/idgen"
)

// offset cursors carry an opaque base64 of "o:<offset>". They're used where
// the result ordering is volatile (e.g. live state and latest messages), so a
// keyset cursor over a stable key isn't possible. A bad/missing cursor decodes
// to 0.
func encodeOffsetCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o:" + strconv.Itoa(offset)))
}

func decodeOffsetCursor(raw string) int {
	if raw == "" {
		return 0
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0
	}
	s := string(decoded)
	if !strings.HasPrefix(s, "o:") {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "o:"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (h *Handler) listRooms(c *gin.Context) {
	userID := currentUserID(c)
	limit := parseLimit(c.Query("limit"), 50, 100)
	// Rooms are ordered by live presence first, then by the latest visible
	// message time. Both keys are volatile, so offset pagination keeps the
	// cursor meaningful within a snapshot without pretending the ordering is
	// immutable. Fetch one extra row to detect whether a further page exists.
	offset := decodeOffsetCursor(c.Query("cursor"))
	fetch := limit + 1
	latestMessageTimeSQL := `(SELECT m.created_at
			   FROM messages m
			   WHERE m.room_id = r.id AND ` + visibleMessageSQL("m") + `
			   ORDER BY m.created_at DESC, m.id DESC
			   LIMIT 1)`

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
			   SELECT COUNT(*) FROM live_participants lp
			   WHERE lp.room_id = r.id AND lp.connection_state != 'left'
			 ) = 0, COALESCE(`+latestMessageTimeSQL+`, r.updated_at) DESC, r.created_at DESC, r.id DESC
			 LIMIT ? OFFSET ?`,
			fetch, offset,
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
			 ORDER BY rm.is_pinned DESC, (
			   SELECT COUNT(*) FROM live_participants lp
			   WHERE lp.room_id = r.id AND lp.connection_state != 'left'
			 ) = 0, COALESCE(CASE WHEN rm.notification_level = 'blocked' THEN NULL ELSE `+latestMessageTimeSQL+` END, r.updated_at) DESC, rm.joined_at DESC, r.id DESC
			 LIMIT ? OFFSET ?`,
			userID, fetch, offset,
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

	var nextCursor any
	if len(rooms) > limit {
		rooms = rooms[:limit]
		nextCursor = encodeOffsetCursor(offset + limit)
	}
	c.JSON(http.StatusOK, gin.H{"rooms": rooms, "next_cursor": nextCursor})
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
	rid, err := idgen.NextRoomRID(h.DB)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to allocate room id")
		return
	}
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
	aiVoiceAnnounceEnabled := false
	if req.AIVoiceAnnounceEnabled != nil {
		aiVoiceAnnounceEnabled = *req.AIVoiceAnnounceEnabled
	}
	if req.AIVoiceAnnouncementsEnabled != nil {
		aiVoiceAnnounceEnabled = *req.AIVoiceAnnouncementsEnabled
	}
	defaultAvatarKey := defaultRoomAvatar(roomID)
	if req.DefaultAvatarKey != nil && strings.TrimSpace(*req.DefaultAvatarKey) != "" {
		defaultAvatarKey = strings.TrimSpace(*req.DefaultAvatarKey)
	}
	var avatarAssetID any
	var avatarURL any
	if req.AvatarAssetID != nil && strings.TrimSpace(*req.AvatarAssetID) != "" {
		assetID := strings.TrimSpace(*req.AvatarAssetID)
		var filename string
		if err := h.DB.QueryRow(`SELECT filename FROM assets WHERE id = ? AND owner_user_id = ?`, assetID, userID).Scan(&filename); err != nil {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "avatar asset not found")
			return
		}
		avatarAssetID = assetID
		avatarURL = h.assetStore().PublicURL(h.assetStore().ObjectKey(assetID, filename), assetID, filename)
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
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'time_limited', 120, ?, ?, ?)`,
		roomID, rid, name, avatarAssetID, avatarURL, defaultAvatarKey, userID, visibility, joinPolicy, 0, description, now, now,
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
	if err := upsertAIVoiceAnnouncementsPreference(tx, roomID, userID, aiVoiceAnnounceEnabled, now); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to save room preferences")
		return
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
	blacklistFilter := ``
	args := []any{userID, query, query}
	if !superuser {
		blacklistFilter = ` AND NOT EXISTS (
		     SELECT 1
		     FROM room_blacklist rb
		     WHERE rb.room_id = r.id
		       AND rb.user_id = ?
		   )`
		args = append(args, userID)
	}
	args = append(args, limit)
	rows, err := h.DB.Query(
		`SELECT r.id, r.rid, r.name, r.avatar_url, r.default_avatar_key, r.created_by_user_id,
		        r.visibility, r.join_policy, r.ai_voice_announce_enabled,
		        r.message_recall_policy, r.message_recall_window_seconds,
		        r.description, r.created_at, r.updated_at,
		        CASE WHEN rm.user_id IS NULL THEN 0 ELSE 1 END AS joined
		 FROM rooms r
		 LEFT JOIN room_memberships rm ON rm.room_id = r.id AND rm.user_id = ?
		 WHERE `+visibilityFilter+`
		   `+blacklistFilter+`
		 ORDER BY joined DESC, r.updated_at DESC, r.name ASC
		 LIMIT ?`,
		args...,
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
	var role, notificationLevel string
	var remarkName sql.NullString
	var isPinned int
	_ = h.DB.QueryRow(
		`SELECT role, notification_level, remark_name, is_pinned FROM room_memberships WHERE room_id = ? AND user_id = ?`,
		rec.ID, userID,
	).Scan(&role, &notificationLevel, &remarkName, &isPinned)
	if role == "" && h.isSuperuser(userID) {
		role = "superuser"
		notificationLevel = "all"
	}
	lastMessage, err := h.lastMessage(rec.ID)
	if err != nil {
		return roomCard{}, err
	}
	if notificationLevel == "blocked" {
		lastMessage = nil
	}
	aiVoiceAnnouncementsEnabled := h.aiVoiceAnnouncementsEnabled(rec.ID, userID)
	return roomCard{
		ID:                          rec.ID,
		RID:                         rec.RID.String,
		Name:                        rec.Name,
		RemarkName:                  nullableString(remarkName),
		Description:                 rec.Description,
		Visibility:                  rec.Visibility,
		JoinPolicy:                  rec.JoinPolicy,
		AIVoiceAnnounceEnabled:      aiVoiceAnnouncementsEnabled,
		AIVoiceAnnouncementsEnabled: aiVoiceAnnouncementsEnabled,
		AvatarURL:                   nullableString(rec.AvatarURL),
		DefaultAvatarKey:            rec.DefaultAvatarKey,
		MemberCount:                 memberCount,
		MyRole:                      role,
		NotificationLevel:           notificationLevel,
		NotificationPolicy:          notificationLevel,
		IsPinned:                    isPinned != 0,
		OnlineMemberCount:           onlineMemberCount,
		LiveParticipantCount:        liveCount,
		LiveAvatarPreview:           livePreview,
		LastMessage:                 lastMessage,
		UnreadCount:                 h.unreadCount(rec.ID, userID),
		UnreadMentionCount:          h.unreadMentionCount(rec.ID, userID),
		HasPendingJoinRequests: h.hasPendingJoinRequestsForViewer(
			rec.ID,
			userID,
		),
		UpdatedAt: formatMillis(rec.UpdatedAt),
	}, nil
}

func (h *Handler) hasPendingJoinRequestsForViewer(roomID, userID string) bool {
	if roomID == "" || userID == "" || !h.isAdmin(roomID, userID) {
		return false
	}
	var count int
	err := h.DB.QueryRow(
		`SELECT COUNT(*)
		 FROM join_requests jr
		 WHERE jr.room_id = ? AND jr.status = 'pending'
		   AND NOT EXISTS (
		     SELECT 1
		     FROM room_blacklist rb
		     WHERE rb.room_id = jr.room_id
		       AND rb.user_id = jr.user_id
		   )`,
		roomID,
	).Scan(&count)
	return err == nil && count > 0
}

func (h *Handler) buildRoomDetail(roomID, userID string) (roomDetail, error) {
	var rec roomRecord
	var joinedAt int64
	var role, notificationLevel string
	var isPinned int
	var remarkName, roomDisplayName sql.NullString
	err := h.DB.QueryRow(
		`SELECT r.id, r.rid, r.name, r.avatar_url, r.default_avatar_key, r.created_by_user_id,
		        r.visibility, r.join_policy, r.ai_voice_announce_enabled,
		        r.message_recall_policy, r.message_recall_window_seconds,
		        r.description, r.created_at, r.updated_at,
		        rm.role, rm.remark_name, rm.room_display_name,
		        rm.notification_level, rm.is_pinned, rm.joined_at
		 FROM rooms r
		 JOIN room_memberships rm ON rm.room_id = r.id
		 WHERE r.id = ? AND rm.user_id = ?`,
		roomID, userID,
	).Scan(
		&rec.ID, &rec.RID, &rec.Name, &rec.AvatarURL, &rec.DefaultAvatarKey, &rec.CreatedByUserID,
		&rec.Visibility, &rec.JoinPolicy, &rec.AIVoiceAnnounceEnabled, &rec.MessageRecallPolicy,
		&rec.MessageRecallWindowSeconds, &rec.Description, &rec.CreatedAt, &rec.UpdatedAt,
		&role, &remarkName, &roomDisplayName, &notificationLevel, &isPinned, &joinedAt,
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

	aiVoiceAnnouncementsEnabled := h.aiVoiceAnnouncementsEnabled(roomID, userID)
	return roomDetail{
		ID:                          rec.ID,
		RID:                         rec.RID.String,
		Name:                        rec.Name,
		Description:                 rec.Description,
		AvatarURL:                   nullableString(rec.AvatarURL),
		DefaultAvatarKey:            rec.DefaultAvatarKey,
		Visibility:                  rec.Visibility,
		JoinPolicy:                  rec.JoinPolicy,
		AIVoiceAnnounceEnabled:      aiVoiceAnnouncementsEnabled,
		AIVoiceAnnouncementsEnabled: aiVoiceAnnouncementsEnabled,
		MessageRecallPolicy:         rec.MessageRecallPolicy,
		MessageRecallWindowSeconds:  nullableInt64(rec.MessageRecallWindowSeconds),
		MemberCount:                 memberCount,
		OnlineMemberCount:           onlineMemberCount,
		CreatedBy:                   createdBy,
		RemarkName:                  nullableString(remarkName),
		NotificationPolicy:          notificationLevel,
		IsPinned:                    isPinned != 0,
		PersonalProfile: roomPersonalProfile{
			DisplayName: nullableString(roomDisplayName),
		},
		CanDeleteRoom: role == "owner" || role == "superuser" || h.isSuperuser(userID),
		MyMembership: roomMembership{
			Role:                        role,
			JoinedAt:                    formatMillis(joinedAt),
			RemarkName:                  nullableString(remarkName),
			RoomDisplayName:             nullableString(roomDisplayName),
			NotificationLevel:           notificationLevel,
			IsPinned:                    isPinned != 0,
			AIVoiceAnnouncementsEnabled: aiVoiceAnnouncementsEnabled,
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
		`SELECT u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
		        rm.room_display_name, rm.role
		 FROM live_participants lp
		 JOIN users u ON u.id = lp.user_id
		 LEFT JOIN room_memberships rm ON rm.room_id = lp.room_id AND rm.user_id = lp.user_id
		 WHERE lp.room_id = ?
		   AND lp.connection_state != 'left'
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
		var roomDisplayName, roomRole sql.NullString
		if err := rows.Scan(&id, &uid, &username, &displayName, &avatarURL, &defaultAvatar, &roomDisplayName, &roomRole); err != nil {
			return nil, 0, err
		}
		if len(preview) < 5 {
			user := summaryFromUserFields(id, uid, username, displayName, avatarURL, defaultAvatar)
			user.RoomDisplayName = nullableString(roomDisplayName)
			if roomRole.Valid && roomRole.String != "" {
				user.RoomRole = roomRole.String
			}
			preview = append(preview, user)
		}
		count++
	}
	return preview, count, nil
}

func (h *Handler) lastMessage(roomID string) (*lastMessagePreview, error) {
	var id, senderUserID, sender, messageType, body, attachmentsJSON string
	var recalledByUserID, forceDeletedByUserID sql.NullString
	var quoteJSON sql.NullString
	var isRecalled, isForceDeleted bool
	var createdAt int64
	err := h.DB.QueryRow(
		`SELECT m.id,
		        m.sender_user_id,
		        COALESCE(
		          NULLIF(m.sender_room_display_name_snapshot, ''),
		          NULLIF(m.sender_display_name_snapshot, ''),
		          NULLIF(m.sender_username_snapshot, ''),
		          NULLIF(sender_rm.room_display_name, ''),
		          NULLIF(u.display_name, ''),
		          u.username,
		          ''
		        ),
		        m.type, m.body, m.attachments_json, m.quote_json,
		        m.is_recalled, m.recalled_by_user_id,
		        m.is_force_deleted, m.force_deleted_by_user_id,
		        m.created_at
		 FROM messages m
		 LEFT JOIN users u ON u.id = m.sender_user_id
		 LEFT JOIN room_memberships sender_rm ON sender_rm.room_id = m.room_id AND sender_rm.user_id = m.sender_user_id
		 WHERE m.room_id = ? AND `+visibleMessageSQL("m")+`
		 ORDER BY m.created_at DESC, m.id DESC
		 LIMIT 1`,
		roomID,
	).Scan(
		&id, &senderUserID, &sender, &messageType, &body, &attachmentsJSON, &quoteJSON,
		&isRecalled, &recalledByUserID,
		&isForceDeleted, &forceDeletedByUserID,
		&createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	previewSender := sender
	bodyPreview := lastMessageBodyPreview(messageType, body, attachmentsJSON)
	if quoteJSON.Valid && quoteJSON.String != "" {
		bodyPreview = "[引用] " + bodyPreview
	}
	if isRecalled || isForceDeleted {
		previewSender = ""
		messageType = systemMessageType
		bodyPreview = h.removedLastMessagePreview(
			roomID,
			senderUserID,
			sender,
			isRecalled,
			recalledByUserID,
			isForceDeleted,
			forceDeletedByUserID,
		)
	} else if messageType == systemMessageType {
		if systemPreview, ok := systemRoomProfileChangeLastMessagePreview(attachmentsJSON); ok {
			previewSender = ""
			bodyPreview = systemPreview
		}
	}
	return &lastMessagePreview{
		ID:                id,
		Type:              messageType,
		SenderDisplayName: previewSender,
		BodyPreview:       bodyPreview,
		CreatedAt:         formatMillis(createdAt),
	}, nil
}

func (h *Handler) removedLastMessagePreview(
	roomID, senderUserID, senderDisplayName string,
	isRecalled bool,
	recalledByUserID sql.NullString,
	isForceDeleted bool,
	forceDeletedByUserID sql.NullString,
) string {
	if isForceDeleted {
		actorName := ""
		if forceDeletedByUserID.Valid {
			actorName = h.lastMessageUserDisplayName(roomID, forceDeletedByUserID.String)
		}
		if actorName == "" {
			return "消息已被删除"
		}
		return actorName + " 删除了一条消息"
	}

	if !isRecalled {
		return "消息已被删除"
	}
	actorUserID := senderUserID
	if recalledByUserID.Valid && strings.TrimSpace(recalledByUserID.String) != "" {
		actorUserID = recalledByUserID.String
	}
	actorName := senderDisplayName
	if actorUserID != senderUserID {
		actorName = h.lastMessageUserDisplayName(roomID, actorUserID)
	}
	if actorName == "" {
		actorName = "用户"
	}
	if actorUserID == senderUserID {
		return actorName + " 撤回了一条消息"
	}
	if senderDisplayName == "" {
		senderDisplayName = h.lastMessageUserDisplayName(roomID, senderUserID)
	}
	if senderDisplayName == "" {
		senderDisplayName = "用户"
	}
	return actorName + " 撤回了一条来自 " + senderDisplayName + " 的消息"
}

func (h *Handler) lastMessageUserDisplayName(roomID, userID string) string {
	if strings.TrimSpace(userID) == "" {
		return ""
	}
	var displayName string
	err := h.DB.QueryRow(
		`SELECT COALESCE(NULLIF(rm.room_display_name, ''), NULLIF(u.display_name, ''), u.username)
		 FROM users u
		 LEFT JOIN room_memberships rm ON rm.room_id = ? AND rm.user_id = u.id
		 WHERE u.id = ?`,
		roomID, userID,
	).Scan(&displayName)
	if err != nil {
		return ""
	}
	return displayName
}

func systemRoomProfileChangeLastMessagePreview(attachmentsJSON string) (string, bool) {
	for _, raw := range decodeJSONArray(attachmentsJSON) {
		attachment, ok := raw.(map[string]any)
		if !ok || strings.ToLower(stringFromMap(attachment, "type")) != systemMessageType {
			continue
		}
		event := stringFromMap(attachment, "event")
		if event != systemEventRoomNameChanged &&
			event != systemEventRoomBioChanged &&
			event != systemEventRoomVisibilityChanged &&
			event != systemEventRoomJoinPolicyChanged {
			continue
		}
		subject := "房间名称"
		value := systemChangedValuePreview(stringFromMap(attachment, "new_value"))
		if event == systemEventRoomBioChanged {
			subject = "房间简介"
		} else if event == systemEventRoomVisibilityChanged {
			subject = "房间可见性"
			value = systemVisibilityLabel(stringFromMap(attachment, "new_value"))
		} else if event == systemEventRoomJoinPolicyChanged {
			subject = "房间加入方式"
			value = systemJoinPolicyLabel(stringFromMap(attachment, "new_value"))
		}
		actor := systemAttachmentDisplayName(attachment, "actor")
		if actor == "" {
			actor = systemAttachmentDisplayName(attachment, "user")
		}
		if actor == "" {
			return preview(subject+" 修改为 "+value, 80), true
		}
		return preview(subject+" 被 "+actor+" 修改为 "+value, 80), true
	}
	return "", false
}

func systemAttachmentDisplayName(attachment map[string]any, key string) string {
	user, ok := attachment[key].(map[string]any)
	if !ok {
		return ""
	}
	return firstNonEmptyString(
		stringFromMap(user, "room_display_name"),
		stringFromMap(user, "display_name"),
		stringFromMap(user, "username"),
	)
}

func systemChangedValuePreview(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "（空）"
	}
	return strings.Join(strings.Fields(value), " ")
}

func lastMessageBodyPreview(messageType, body, attachmentsJSON string) string {
	attachments := decodeJSONArray(attachmentsJSON)
	hasAudio := messageType == "audio"
	audioDurationMS := int64(0)
	hasImage := false
	hasFile := messageType == "file"
	hasNonImageFile := false
	imageName := ""
	fileName := ""
	stickerName := ""
	for _, raw := range attachments {
		attachment, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		attachmentType := strings.ToLower(stringFromMap(attachment, "type"))
		mimeType := strings.ToLower(attachmentMimeType(attachment))
		displayName := attachmentDisplayName(attachment)
		if attachmentType == "audio" || strings.HasPrefix(mimeType, "audio/") {
			hasAudio = true
			if audioDurationMS == 0 {
				audioDurationMS = int64FromMap(attachment, "duration_ms")
			}
		}
		if attachmentType == "sticker" && stickerName == "" {
			stickerName = displayName
		}
		if attachmentType == "file" {
			hasFile = true
			if strings.HasPrefix(mimeType, "image/") {
				hasImage = true
				if imageName == "" {
					imageName = displayName
				}
			} else {
				hasNonImageFile = true
				if fileName == "" {
					fileName = displayName
				}
			}
		}
	}
	if hasAudio {
		return labelledLastMessagePreview("[语音]", formatVoicePreviewDuration(audioDurationMS))
	}
	if messageType == "sticker" || stickerName != "" {
		return labelledLastMessagePreview("[表情]", firstNonEmptyString(stickerName, stickerNameFromBody(body)))
	}
	if hasFile {
		if hasImage && !hasNonImageFile {
			return labelledLastMessagePreview("[图片]", firstNonEmptyString(imageName, body))
		}
		return labelledLastMessagePreview("[文件]", firstNonEmptyString(fileName, imageName, body))
	}
	return preview(body, 80)
}

func formatVoicePreviewDuration(durationMS int64) string {
	if durationMS <= 0 {
		return ""
	}
	totalSeconds := (durationMS + 999) / 1000
	if totalSeconds < 60 {
		return strconv.FormatInt(totalSeconds, 10) + `"`
	}
	minutes := totalSeconds / 60
	seconds := totalSeconds % 60
	secondsText := strconv.FormatInt(seconds, 10)
	if seconds < 10 {
		secondsText = "0" + secondsText
	}
	return strconv.FormatInt(minutes, 10) + "'" + secondsText + `"`
}

func labelledLastMessagePreview(label, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return label
	}
	return preview(label+" "+name, 80)
}

func stickerNameFromBody(body string) string {
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "[表情]") {
		return strings.TrimSpace(strings.TrimPrefix(trimmed, "[表情]"))
	}
	if strings.HasPrefix(trimmed, "[") && strings.Contains(trimmed, "]") {
		end := strings.Index(trimmed, "]")
		return strings.TrimSpace(trimmed[1:end])
	}
	return trimmed
}

func attachmentDisplayName(attachment map[string]any) string {
	if name := stringFromMap(attachment, "name"); name != "" {
		return name
	}
	if filename := stringFromMap(attachment, "filename"); filename != "" {
		return filename
	}
	asset, ok := attachment["asset"].(map[string]any)
	if !ok {
		return ""
	}
	return stringFromMap(asset, "filename")
}

func attachmentMimeType(attachment map[string]any) string {
	if mimeType := stringFromMap(attachment, "mime_type"); mimeType != "" {
		return mimeType
	}
	asset, ok := attachment["asset"].(map[string]any)
	if !ok {
		return ""
	}
	return stringFromMap(asset, "mime_type")
}

func stringFromMap(values map[string]any, key string) string {
	value, ok := values[key].(string)
	if !ok {
		return ""
	}
	return value
}

func int64FromMap(values map[string]any, key string) int64 {
	switch value := values[key].(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	case string:
		parsed, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		return parsed
	default:
		return 0
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (h *Handler) unreadCount(roomID, userID string) int {
	var notificationLevel string
	_ = h.DB.QueryRow(
		`SELECT notification_level FROM room_memberships WHERE room_id = ? AND user_id = ?`,
		roomID, userID,
	).Scan(&notificationLevel)
	if notificationLevel == "blocked" {
		return 0
	}

	var readAt int64
	var readMessageID string
	_ = h.DB.QueryRow(
		`SELECT m.created_at, m.id
		 FROM room_reads rr
		 JOIN messages m ON m.id = rr.last_read_message_id
		 WHERE rr.room_id = ? AND rr.user_id = ?`,
		roomID, userID,
	).Scan(&readAt, &readMessageID)

	var count int
	_ = h.DB.QueryRow(
		`SELECT COUNT(*) FROM messages m
		 WHERE m.room_id = ? AND m.sender_user_id != ?
		   AND (m.created_at > ? OR (m.created_at = ? AND m.id > ?))
		   AND `+visibleMessageSQL("m"),
		roomID, userID, readAt, readAt, readMessageID,
	).Scan(&count)
	return count
}

func (h *Handler) unreadMentionCount(roomID, userID string) int {
	var notificationLevel string
	_ = h.DB.QueryRow(
		`SELECT notification_level FROM room_memberships WHERE room_id = ? AND user_id = ?`,
		roomID, userID,
	).Scan(&notificationLevel)
	if notificationLevel == "blocked" {
		return 0
	}

	target, ok := h.mentionTarget(roomID, userID)
	if !ok {
		return 0
	}

	var readAt int64
	var readMessageID string
	_ = h.DB.QueryRow(
		`SELECT m.created_at, m.id
		 FROM room_reads rr
		 JOIN messages m ON m.id = rr.last_read_message_id
		 WHERE rr.room_id = ? AND rr.user_id = ?`,
		roomID, userID,
	).Scan(&readAt, &readMessageID)

	rows, err := h.DB.Query(
		`SELECT m.body, m.mentions_json
		 FROM messages m
		 WHERE m.room_id = ? AND m.sender_user_id != ?
		   AND (m.created_at > ? OR (m.created_at = ? AND m.id > ?))
		   AND m.is_recalled = 0 AND m.is_force_deleted = 0
		   AND `+visibleMessageSQL("m"),
		roomID, userID, readAt, readAt, readMessageID,
	)
	if err != nil {
		return 0
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var body, mentionsJSON string
		if err := rows.Scan(&body, &mentionsJSON); err != nil {
			return count
		}
		if mentionMessageTargetsUser(body, mentionsJSON, target) {
			count++
		}
	}
	return count
}

type mentionTarget struct {
	UserID string
	UID    string
	Labels map[string]struct{}
	Admin  bool
}

func (h *Handler) mentionTarget(roomID, userID string) (mentionTarget, bool) {
	var uid, username string
	var displayName, roomDisplayName sql.NullString
	var role sql.NullString
	var isSuperuser int
	err := h.DB.QueryRow(
		`SELECT u.id, u.uid, u.username, u.display_name, rm.room_display_name,
		        rm.role, u.is_superuser
		 FROM users u
		 LEFT JOIN room_memberships rm ON rm.room_id = ? AND rm.user_id = u.id
		 WHERE u.id = ?`,
		roomID, userID,
	).Scan(&userID, &uid, &username, &displayName, &roomDisplayName, &role, &isSuperuser)
	if err != nil {
		return mentionTarget{}, false
	}
	target := mentionTarget{
		UserID: userID,
		UID:    uid,
		Labels: map[string]struct{}{},
		Admin:  isSuperuser != 0 || mentionRoleIsAdmin(role.String),
	}
	for _, value := range []string{
		userID,
		uid,
		username,
		displayName.String,
		roomDisplayName.String,
	} {
		if label := normalizeMentionLabel(value); label != "" {
			target.Labels[label] = struct{}{}
		}
	}
	return target, true
}

func mentionMessageTargetsUser(body, mentionsJSON string, target mentionTarget) bool {
	for _, raw := range decodeJSONArray(mentionsJSON) {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(stringFromMap(item, "type"))) {
		case "all":
			return true
		case "admins":
			if target.Admin {
				return true
			}
		case "user":
			userID := strings.TrimSpace(stringFromMap(item, "user_id"))
			uid := strings.TrimSpace(stringFromMap(item, "uid"))
			if userID != "" && userID == target.UserID {
				return true
			}
			if uid != "" && uid == target.UID {
				return true
			}
		}
	}

	if mentionTextTargetsKnownLabel(body, target) {
		return true
	}

	for _, token := range mentionTextTokens(body) {
		normalized := normalizeMentionLabel(token)
		switch normalized {
		case normalizeMentionLabel("所有人"):
			return true
		case normalizeMentionLabel("管理员"):
			if target.Admin {
				return true
			}
		default:
			if _, ok := target.Labels[normalized]; ok {
				return true
			}
		}
	}
	return false
}

func mentionTextTargetsKnownLabel(body string, target mentionTarget) bool {
	if len(target.Labels) == 0 {
		return false
	}
	runes := []rune(body)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '@' {
			continue
		}
		start := i + 1
		if start >= len(runes) {
			continue
		}
		remainder := strings.ToLower(string(runes[start:]))
		for label := range target.Labels {
			labelRunes := []rune(label)
			if label == "" || len(labelRunes) > len(runes)-start {
				continue
			}
			if !strings.HasPrefix(remainder, label) {
				continue
			}
			end := start + len(labelRunes)
			if end < len(runes) && !isMentionTerminatorRune(runes[end]) {
				continue
			}
			return true
		}
	}
	return false
}

func mentionTextTokens(value string) []string {
	runes := []rune(value)
	tokens := make([]string, 0)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '@' {
			continue
		}
		start := i + 1
		end := start
		for end < len(runes) && !isMentionTerminatorRune(runes[end]) {
			if runes[end] == '@' {
				break
			}
			end++
		}
		if end <= start {
			continue
		}
		token := string(runes[start:end])
		if looksLikeEmailMention(runes, i, end) {
			continue
		}
		tokens = append(tokens, token)
		i = end - 1
	}
	return tokens
}

func isMentionTerminatorRune(value rune) bool {
	return unicode.IsSpace(value) || value == '\u3000'
}

func looksLikeEmailMention(runes []rune, atIndex, end int) bool {
	if atIndex <= 0 || end <= atIndex+1 {
		return false
	}
	domain := string(runes[atIndex+1 : end])
	if !strings.Contains(domain, ".") {
		return false
	}
	localStart := atIndex - 1
	for localStart >= 0 && isEmailLocalRune(runes[localStart]) {
		localStart--
	}
	localStart++
	if localStart >= atIndex {
		return false
	}
	for _, r := range runes[atIndex+1 : end] {
		if !isEmailDomainRune(r) {
			return false
		}
	}
	return true
}

func isEmailLocalRune(value rune) bool {
	return (value >= '0' && value <= '9') ||
		(value >= 'A' && value <= 'Z') ||
		(value >= 'a' && value <= 'z') ||
		value == '.' || value == '_' || value == '%' ||
		value == '+' || value == '-'
}

func isEmailDomainRune(value rune) bool {
	return (value >= '0' && value <= '9') ||
		(value >= 'A' && value <= 'Z') ||
		(value >= 'a' && value <= 'z') ||
		value == '.' || value == '-'
}

func normalizeMentionLabel(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func mentionRoleIsAdmin(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "owner", "creator", "admin", "administrator", "superuser":
		return true
	default:
		return false
	}
}
