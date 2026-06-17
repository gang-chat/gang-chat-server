package chat

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (h *Handler) searchAll(c *gin.Context) {
	query := strings.TrimSpace(c.Query("q"))
	if query == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "search keyword is required")
		return
	}

	userID := currentUserID(c)
	limit := parseLimit(c.Query("limit"), 8, 20)

	myRooms, err := h.searchMyRooms(userID, query, limit)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to search my rooms")
		return
	}
	publicRooms, err := h.searchPublicRooms(userID, query, limit)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to search public rooms")
		return
	}
	messages, err := h.searchMessages(userID, query, limit)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to search messages")
		return
	}
	files, err := h.searchFiles(userID, query, limit)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to search files")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"my_rooms":     myRooms,
		"public_rooms": publicRooms,
		"messages":     messages,
		"files":        files,
	})
}

func (h *Handler) searchMyRooms(userID, query string, limit int) ([]roomCard, error) {
	var rows *sql.Rows
	var err error
	if h.isSuperuser(userID) {
		rows, err = h.DB.Query(
			`SELECT r.id, r.rid, r.name, r.avatar_url, r.default_avatar_key, r.created_by_user_id,
			        r.visibility, r.join_policy, r.ai_voice_announce_enabled,
			        r.message_recall_policy, r.message_recall_window_seconds,
			        r.description, r.created_at, r.updated_at
			 FROM rooms r
			 WHERE r.rid = ?
			    OR instr(lower(r.name), lower(?)) > 0
			 ORDER BY r.updated_at DESC, r.created_at DESC
			 LIMIT ?`,
			query, query, limit,
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
			   AND (
			     r.rid = ?
			     OR instr(lower(r.name), lower(?)) > 0
			     OR instr(lower(COALESCE(rm.remark_name, '')), lower(?)) > 0
			     OR instr(lower(COALESCE(rm.room_display_name, '')), lower(?)) > 0
			   )
			 ORDER BY r.updated_at DESC, rm.joined_at DESC
			 LIMIT ?`,
			userID, query, query, query, query, limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	rooms := make([]roomCard, 0)
	for rows.Next() {
		rec, err := scanRoomRecord(rows)
		if err != nil {
			return nil, err
		}
		card, err := h.buildRoomCard(rec, userID)
		if err != nil {
			return nil, err
		}
		rooms = append(rooms, card)
	}
	return rooms, rows.Err()
}

func (h *Handler) searchPublicRooms(userID, query string, limit int) ([]publicRoom, error) {
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
		   AND rm.user_id IS NULL
		 ORDER BY r.updated_at DESC, r.name ASC
		 LIMIT ?`,
		userID, query, query, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	rooms := make([]publicRoom, 0)
	for rows.Next() {
		rec, joined, err := scanPublicRoomRecord(rows)
		if err != nil {
			return nil, err
		}
		memberCount, err := h.memberCount(rec.ID)
		if err != nil {
			return nil, err
		}
		onlineMemberCount, err := h.onlineMemberCount(rec.ID)
		if err != nil {
			return nil, err
		}
		_, liveCount, err := h.livePreview(rec.ID)
		if err != nil {
			return nil, err
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
	return rooms, rows.Err()
}

func (h *Handler) searchMessages(userID, query string, limit int) ([]messageSearchResult, error) {
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
	)
}

func (h *Handler) searchFiles(userID, query string, limit int) ([]messageSearchResult, error) {
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
	)
}

func (h *Handler) searchMessageRows(userID, predicate string, predicateArgs []any, limit int) ([]messageSearchResult, error) {
	accessJoin := `JOIN room_memberships rm ON rm.room_id = m.room_id AND rm.user_id = ?`
	args := []any{userID}
	if h.isSuperuser(userID) {
		accessJoin = ``
		args = []any{}
	}
	args = append(args, predicateArgs...)
	args = append(args, limit)

	rows, err := h.DB.Query(
		`SELECT m.id, m.room_id, m.client_message_id, m.type, m.body,
		        m.mentions_json, m.attachments_json, m.is_recalled, m.recalled_at,
		        m.recalled_by_user_id, m.is_force_deleted, m.force_deleted_at,
		        m.force_deleted_by_user_id, m.created_at,
		        u.id, u.uid, u.username, u.display_name, u.avatar_url, u.default_avatar_key,
		        sender_rm.room_display_name,
		        r.id, r.rid, r.name, r.avatar_url, r.default_avatar_key
		 FROM messages m
		 JOIN users u ON u.id = m.sender_user_id
		 JOIN rooms r ON r.id = m.room_id
		 LEFT JOIN room_memberships sender_rm ON sender_rm.room_id = m.room_id AND sender_rm.user_id = m.sender_user_id
		 `+accessJoin+`
		 WHERE m.is_recalled = 0
		   AND m.is_force_deleted = 0
		   AND `+predicate+`
		 ORDER BY m.created_at DESC
		 LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := make([]messageSearchResult, 0)
	for rows.Next() {
		msg, room, err := scanSearchMessage(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, messageSearchResult{
			Room:    room,
			Message: msg,
		})
	}
	return results, rows.Err()
}

func scanSearchMessage(rows *sql.Rows) (message, searchRoomContext, error) {
	var msg message
	var room searchRoomContext
	var senderID, senderUID, senderUsername string
	var senderDisplayName, senderAvatarURL, senderDefaultAvatar, senderRoomDisplayName sql.NullString
	var roomRID, roomAvatarURL, roomDefaultAvatar sql.NullString
	var mentionsJSON, attachmentsJSON string
	var recalledAt, forceDeletedAt sql.NullInt64
	var recalledByUserID, forceDeletedByUserID sql.NullString
	var isRecalled, isForceDeleted int
	var createdAt int64
	if err := rows.Scan(
		&msg.ID, &msg.RoomID, &msg.ClientMessageID, &msg.Type, &msg.Body,
		&mentionsJSON, &attachmentsJSON, &isRecalled, &recalledAt, &recalledByUserID,
		&isForceDeleted, &forceDeletedAt, &forceDeletedByUserID, &createdAt,
		&senderID, &senderUID, &senderUsername, &senderDisplayName, &senderAvatarURL, &senderDefaultAvatar,
		&senderRoomDisplayName,
		&room.ID, &roomRID, &room.Name, &roomAvatarURL, &roomDefaultAvatar,
	); err != nil {
		return message{}, searchRoomContext{}, err
	}
	msg.Sender = summaryFromUserFields(senderID, senderUID, senderUsername, senderDisplayName, senderAvatarURL, senderDefaultAvatar)
	if senderRoomDisplayName.Valid && senderRoomDisplayName.String != "" {
		msg.Sender.RoomDisplayName = &senderRoomDisplayName.String
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
	room.DefaultAvatarKey = "room-1"
	if roomDefaultAvatar.Valid && roomDefaultAvatar.String != "" {
		room.DefaultAvatarKey = roomDefaultAvatar.String
	}
	return msg, room, nil
}
