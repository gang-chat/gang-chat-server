package chat

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	searchCategoryMyRooms     = "my_rooms"
	searchCategoryPublicRooms = "public_rooms"
	searchCategoryMessages    = "messages"
	searchCategoryFiles       = "files"

	searchParamMyRoomsCursor     = "my_rooms_cursor"
	searchParamPublicRoomsCursor = "public_rooms_cursor"
	searchParamMessagesCursor    = "messages_cursor"
	searchParamFilesCursor       = "files_cursor"
)

type searchCategorySet struct {
	myRooms     bool
	publicRooms bool
	messages    bool
	files       bool
}

func allSearchCategories() searchCategorySet {
	return searchCategorySet{
		myRooms:     true,
		publicRooms: true,
		messages:    true,
		files:       true,
	}
}

func parseSearchCategories(raw string) searchCategorySet {
	if raw == "" {
		return allSearchCategories()
	}

	var categories searchCategorySet
	valid := false
	for _, item := range strings.Split(raw, ",") {
		switch strings.TrimSpace(item) {
		case searchCategoryMyRooms:
			categories.myRooms = true
			valid = true
		case searchCategoryPublicRooms:
			categories.publicRooms = true
			valid = true
		case searchCategoryMessages:
			categories.messages = true
			valid = true
		case searchCategoryFiles:
			categories.files = true
			valid = true
		}
	}
	if !valid {
		return allSearchCategories()
	}
	return categories
}

func (h *Handler) searchAll(c *gin.Context) {
	query := strings.TrimSpace(c.Query("q"))
	if query == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "search keyword is required")
		return
	}

	userID := currentUserID(c)
	limit := parseLimit(c.Query("limit"), 8, 20)
	categories := parseSearchCategories(c.Query("categories"))
	nextCursors := gin.H{
		searchCategoryMyRooms:     nil,
		searchCategoryPublicRooms: nil,
		searchCategoryMessages:    nil,
		searchCategoryFiles:       nil,
	}
	totalCounts := gin.H{
		searchCategoryMyRooms:     0,
		searchCategoryPublicRooms: 0,
		searchCategoryMessages:    0,
		searchCategoryFiles:       0,
	}

	myRooms := make([]roomCard, 0)
	publicRooms := make([]publicRoom, 0)
	messages := make([]messageSearchResult, 0)
	files := make([]messageSearchResult, 0)
	var err error
	var nextCursor any
	var totalCount int

	if categories.myRooms {
		myRooms, nextCursor, totalCount, err = h.searchMyRooms(userID, query, limit, decodeOffsetCursor(c.Query(searchParamMyRoomsCursor)))
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to search my rooms")
			return
		}
		nextCursors[searchCategoryMyRooms] = nextCursor
		totalCounts[searchCategoryMyRooms] = totalCount
	}
	if categories.publicRooms {
		publicRooms, nextCursor, totalCount, err = h.searchPublicRooms(userID, query, limit, decodeOffsetCursor(c.Query(searchParamPublicRoomsCursor)))
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to search public rooms")
			return
		}
		nextCursors[searchCategoryPublicRooms] = nextCursor
		totalCounts[searchCategoryPublicRooms] = totalCount
	}
	if categories.messages {
		messages, nextCursor, totalCount, err = h.searchMessages(userID, query, limit, decodeOffsetCursor(c.Query(searchParamMessagesCursor)))
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to search messages")
			return
		}
		nextCursors[searchCategoryMessages] = nextCursor
		totalCounts[searchCategoryMessages] = totalCount
	}
	if categories.files {
		files, nextCursor, totalCount, err = h.searchFiles(userID, query, limit, decodeOffsetCursor(c.Query(searchParamFilesCursor)))
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to search files")
			return
		}
		nextCursors[searchCategoryFiles] = nextCursor
		totalCounts[searchCategoryFiles] = totalCount
	}

	c.JSON(http.StatusOK, gin.H{
		searchCategoryMyRooms:     myRooms,
		searchCategoryPublicRooms: publicRooms,
		searchCategoryMessages:    messages,
		searchCategoryFiles:       files,
		"next_cursors":            nextCursors,
		"total_counts":            totalCounts,
	})
}

func (h *Handler) searchMyRooms(userID, query string, limit, offset int) ([]roomCard, any, int, error) {
	fetch := limit + 1
	var total int
	var rows *sql.Rows
	var err error
	if h.isSuperuser(userID) {
		if err := h.DB.QueryRow(
			`SELECT COUNT(*)
			 FROM rooms r
			 WHERE r.rid = ?
			    OR instr(lower(r.name), lower(?)) > 0`,
			query, query,
		).Scan(&total); err != nil {
			return nil, nil, 0, err
		}
		rows, err = h.DB.Query(
			`SELECT r.id, r.rid, r.name, r.avatar_url, r.default_avatar_key, r.created_by_user_id,
			        r.visibility, r.join_policy, r.ai_voice_announce_enabled,
			        r.message_recall_policy, r.message_recall_window_seconds,
			        r.description, r.created_at, r.updated_at
			 FROM rooms r
			 WHERE r.rid = ?
			    OR instr(lower(r.name), lower(?)) > 0
			 ORDER BY r.updated_at DESC, r.created_at DESC, r.id DESC
			 LIMIT ? OFFSET ?`,
			query, query, fetch, offset,
		)
	} else {
		if err := h.DB.QueryRow(
			`SELECT COUNT(*)
			 FROM rooms r
			 JOIN room_memberships rm ON rm.room_id = r.id
			 WHERE rm.user_id = ?
			   AND (
			     r.rid = ?
			     OR instr(lower(r.name), lower(?)) > 0
			     OR instr(lower(COALESCE(rm.remark_name, '')), lower(?)) > 0
			     OR instr(lower(COALESCE(rm.room_display_name, '')), lower(?)) > 0
			   )`,
			userID, query, query, query, query,
		).Scan(&total); err != nil {
			return nil, nil, 0, err
		}
		rows, err = h.DB.Query(
			`SELECT r.id, r.rid, r.name, r.avatar_url, r.default_avatar_key, r.created_by_user_id,
			        r.visibility, r.join_policy, r.ai_voice_announce_enabled,
			        r.message_recall_policy, r.message_recall_window_seconds,
			        r.description, r.created_at, r.updated_at
			 FROM rooms r
			 JOIN room_memberships rm ON rm.room_id = r.id
			 WHERE rm.user_id = ?
			   AND (
			     r.rid = ?
			     OR instr(lower(r.name), lower(?)) > 0
			     OR instr(lower(COALESCE(rm.remark_name, '')), lower(?)) > 0
			     OR instr(lower(COALESCE(rm.room_display_name, '')), lower(?)) > 0
			   )
			 ORDER BY r.updated_at DESC, rm.joined_at DESC, r.id DESC
			 LIMIT ? OFFSET ?`,
			userID, query, query, query, query, fetch, offset,
		)
	}
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()

	rooms := make([]roomCard, 0)
	for rows.Next() {
		rec, err := scanRoomRecord(rows)
		if err != nil {
			return nil, nil, 0, err
		}
		card, err := h.buildRoomCard(rec, userID)
		if err != nil {
			return nil, nil, 0, err
		}
		rooms = append(rooms, card)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}
	rooms, nextCursor := trimSearchPage(rooms, limit, offset)
	return rooms, nextCursor, total, nil
}

func (h *Handler) searchPublicRooms(userID, query string, limit, offset int) ([]publicRoom, any, int, error) {
	fetch := limit + 1
	superuser := h.isSuperuser(userID)
	visibilityFilter := `(r.rid = ? OR (r.visibility = 'public' AND instr(lower(r.name), lower(?)) > 0))`
	if superuser {
		visibilityFilter = `(r.rid = ? OR instr(lower(r.name), lower(?)) > 0)`
	}
	var total int
	if err := h.DB.QueryRow(
		`SELECT COUNT(*)
		 FROM rooms r
		 LEFT JOIN room_memberships rm ON rm.room_id = r.id AND rm.user_id = ?
		 WHERE `+visibilityFilter+`
		   AND rm.user_id IS NULL`,
		userID, query, query,
	).Scan(&total); err != nil {
		return nil, nil, 0, err
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
		   AND rm.user_id IS NULL
		 ORDER BY r.updated_at DESC, r.name ASC, r.id DESC
		 LIMIT ? OFFSET ?`,
		userID, query, query, fetch, offset,
	)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()

	rooms := make([]publicRoom, 0)
	for rows.Next() {
		rec, joined, err := scanPublicRoomRecord(rows)
		if err != nil {
			return nil, nil, 0, err
		}
		memberCount, err := h.memberCount(rec.ID)
		if err != nil {
			return nil, nil, 0, err
		}
		onlineMemberCount, err := h.onlineMemberCount(rec.ID)
		if err != nil {
			return nil, nil, 0, err
		}
		_, liveCount, err := h.livePreview(rec.ID)
		if err != nil {
			return nil, nil, 0, err
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
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}
	rooms, nextCursor := trimSearchPage(rooms, limit, offset)
	return rooms, nextCursor, total, nil
}

func (h *Handler) searchMessages(userID, query string, limit, offset int) ([]messageSearchResult, any, int, error) {
	return h.searchMessageRows(
		userID,
		`m.type NOT IN ('file', 'audio', 'system')
		  AND (
		    instr(lower(m.body), lower(?)) > 0
		    OR instr(lower(r.name), lower(?)) > 0
		    OR COALESCE(r.rid, '') = ?
		    OR instr(lower(u.username), lower(?)) > 0
		    OR instr(lower(COALESCE(u.display_name, '')), lower(?)) > 0
		    OR COALESCE(u.uid, '') = ?
		    OR instr(lower(COALESCE(sender_rm.room_display_name, '')), lower(?)) > 0
		  )`,
		[]any{query, query, query, query, query, query, query},
		limit,
		offset,
	)
}

func (h *Handler) searchFiles(userID, query string, limit, offset int) ([]messageSearchResult, any, int, error) {
	return h.searchMessageRows(
		userID,
		`EXISTS (
		    SELECT 1
		    FROM json_each(m.attachments_json) attachment
		    WHERE (m.type = 'file' OR lower(COALESCE(json_extract(attachment.value, '$.type'), '')) = 'file')
		      AND (
		        instr(lower(COALESCE(json_extract(attachment.value, '$.name'), '')), lower(?)) > 0
		        OR instr(lower(COALESCE(json_extract(attachment.value, '$.filename'), '')), lower(?)) > 0
		        OR instr(lower(COALESCE(json_extract(attachment.value, '$.asset.filename'), '')), lower(?)) > 0
		      )
		  )`,
		[]any{query, query, query},
		limit,
		offset,
	)
}

func (h *Handler) searchMessageRows(userID, predicate string, predicateArgs []any, limit, offset int) ([]messageSearchResult, any, int, error) {
	fetch := limit + 1
	accessJoin := `JOIN room_memberships rm ON rm.room_id = m.room_id AND rm.user_id = ?`
	args := []any{userID}
	if h.isSuperuser(userID) {
		accessJoin = ``
		args = []any{}
	}
	args = append(args, predicateArgs...)

	var total int
	if err := h.DB.QueryRow(
		`SELECT COUNT(*)
		 FROM messages m
		 JOIN users u ON u.id = m.sender_user_id
		 JOIN rooms r ON r.id = m.room_id
		 LEFT JOIN room_memberships sender_rm ON sender_rm.room_id = m.room_id AND sender_rm.user_id = m.sender_user_id
		 `+accessJoin+`
		 WHERE m.is_recalled = 0
		   AND m.is_force_deleted = 0
		   AND `+predicate,
		args...,
	).Scan(&total); err != nil {
		return nil, nil, 0, err
	}
	args = append(args, fetch, offset)

	rows, err := h.DB.Query(
		`SELECT m.id, m.room_id, m.client_message_id, m.type, m.body,
		        m.mentions_json, m.attachments_json, m.is_recalled, m.recalled_at,
		        m.recalled_by_user_id, m.is_force_deleted, m.force_deleted_at,
		        m.force_deleted_by_user_id, m.created_at,
		        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
		        u.is_superuser, sender_rm.room_display_name,
		        CASE WHEN u.is_superuser != 0 THEN 'superuser' ELSE COALESCE(sender_rm.role, '') END,
		        r.id, r.rid, r.name, r.avatar_url, r.default_avatar_key
		 FROM messages m
		 JOIN users u ON u.id = m.sender_user_id
		 JOIN rooms r ON r.id = m.room_id
		 LEFT JOIN room_memberships sender_rm ON sender_rm.room_id = m.room_id AND sender_rm.user_id = m.sender_user_id
		 `+accessJoin+`
		 WHERE m.is_recalled = 0
		   AND m.is_force_deleted = 0
		   AND `+predicate+`
		 ORDER BY m.created_at DESC, m.id DESC
		 LIMIT ? OFFSET ?`,
		args...,
	)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()

	results := make([]messageSearchResult, 0)
	for rows.Next() {
		msg, room, err := scanSearchMessage(rows)
		if err != nil {
			return nil, nil, 0, err
		}
		results = append(results, messageSearchResult{
			Room:    room,
			Message: msg,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}
	results, nextCursor := trimSearchPage(results, limit, offset)
	return results, nextCursor, total, nil
}

func trimSearchPage[T any](results []T, limit, offset int) ([]T, any) {
	if len(results) <= limit {
		return results, nil
	}
	return results[:limit], encodeOffsetCursor(offset + limit)
}

func scanSearchMessage(rows *sql.Rows) (message, searchRoomContext, error) {
	var msg message
	var room searchRoomContext
	var senderID, senderUID, senderUsername string
	var senderDisplayName, senderAvatarURL, senderDefaultAvatar, senderRoomDisplayName, senderRoomRole sql.NullString
	var roomRID, roomAvatarURL, roomDefaultAvatar sql.NullString
	var mentionsJSON, attachmentsJSON string
	var recalledAt, forceDeletedAt sql.NullInt64
	var recalledByUserID, forceDeletedByUserID sql.NullString
	var isRecalled, isForceDeleted, senderIsSuperuser int
	var createdAt int64
	if err := rows.Scan(
		&msg.ID, &msg.RoomID, &msg.ClientMessageID, &msg.Type, &msg.Body,
		&mentionsJSON, &attachmentsJSON, &isRecalled, &recalledAt, &recalledByUserID,
		&isForceDeleted, &forceDeletedAt, &forceDeletedByUserID, &createdAt,
		&senderID, &senderUID, &senderUsername, &senderDisplayName, &senderAvatarURL, &senderDefaultAvatar,
		&senderIsSuperuser, &senderRoomDisplayName, &senderRoomRole,
		&room.ID, &roomRID, &room.Name, &roomAvatarURL, &roomDefaultAvatar,
	); err != nil {
		return message{}, searchRoomContext{}, err
	}
	msg.Sender = summaryFromUserFields(senderID, senderUID, senderUsername, senderDisplayName, senderAvatarURL, senderDefaultAvatar)
	msg.Sender.IsSuperuser = senderIsSuperuser != 0
	if senderRoomDisplayName.Valid && senderRoomDisplayName.String != "" {
		msg.Sender.RoomDisplayName = &senderRoomDisplayName.String
	}
	if senderRoomRole.Valid && senderRoomRole.String != "" {
		msg.Sender.RoomRole = senderRoomRole.String
	}
	msg.Mentions = decodeJSONArray(mentionsJSON)
	msg.Attachments = decodeJSONArray(attachmentsJSON)
	msg.IsRecalled = isRecalled != 0
	msg.IsForceDeleted = isForceDeleted != 0
	if recalledAt.Valid {
		v := formatMillis(recalledAt.Int64)
		msg.RecalledAt = &v
	}
	if forceDeletedAt.Valid {
		v := formatMillis(forceDeletedAt.Int64)
		msg.ForceDeletedAt = &v
	}
	msg.CreatedAt = formatMillis(createdAt)

	room.RID = roomRID.String
	room.AvatarURL = nullableString(roomAvatarURL)
	room.DefaultAvatarKey = "blue-3"
	if roomDefaultAvatar.Valid && roomDefaultAvatar.String != "" {
		room.DefaultAvatarKey = roomDefaultAvatar.String
	}
	return msg, room, nil
}
