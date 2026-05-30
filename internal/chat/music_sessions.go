package chat

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
)

type musicSessionRecord struct {
	ID             string
	RoomID         string
	UserID         string
	State          string
	Mode           string
	PlaylistID     sql.NullString
	PlaylistScope  sql.NullString
	CurrentQueueID sql.NullString
	FollowUserID   sql.NullString
	PositionMS     int64
	StartedAt      sql.NullInt64
	UpdatedAt      int64
}

func (h *Handler) controlMusicSession(c *gin.Context) {
	roomID := c.Param("room_id")
	userID := currentUserID(c)
	if !h.requireMember(c, roomID) {
		return
	}

	var req musicSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil || !allowed(req.Action, "play", "pause", "stop", "seek", "previous", "next", "set_mode", "select_playlist", "follow_user", "leave_follow") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid music session action")
		return
	}

	session, err := h.ensureMusicSession(roomID, userID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to prepare music session")
		return
	}

	now := nowMillis()
	switch req.Action {
	case "play":
		queueID := req.QueueID
		if queueID == "" {
			queueID = nullableStringValue(session.CurrentQueueID)
		}
		if queueID == "" {
			queueID, _ = h.firstMusicQueueID(roomID)
		}
		if queueID == "" {
			h.jsonError(c, http.StatusConflict, "conflict", "music queue is empty")
			return
		}
		if queueID != "" && !h.roomOwnsQueueItem(roomID, queueID) {
			h.jsonError(c, http.StatusNotFound, "not_found", "queue item not found")
			return
		}
		position := session.PositionMS
		if req.PositionMS != nil {
			position = *req.PositionMS
		}
		_, err = h.DB.Exec(
			`UPDATE music_sessions
			 SET state = 'playing', current_queue_id = NULLIF(?, ''), follow_user_id = NULL,
			     position_ms = ?, started_at = ?, updated_at = ?
			 WHERE id = ?`,
			queueID, position, now, now, session.ID,
		)
	case "pause":
		position := session.PositionMS
		if req.PositionMS != nil {
			position = *req.PositionMS
		}
		_, err = h.DB.Exec(
			`UPDATE music_sessions SET state = 'paused', position_ms = ?, updated_at = ? WHERE id = ?`,
			position, now, session.ID,
		)
	case "stop":
		_, err = h.DB.Exec(
			`UPDATE music_sessions
			 SET state = 'stopped', follow_user_id = NULL, position_ms = 0, started_at = NULL, updated_at = ?
			 WHERE id = ?`,
			now, session.ID,
		)
	case "seek":
		if req.PositionMS == nil || *req.PositionMS < 0 {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "position_ms is required")
			return
		}
		_, err = h.DB.Exec(`UPDATE music_sessions SET position_ms = ?, updated_at = ? WHERE id = ?`, *req.PositionMS, now, session.ID)
	case "previous", "next":
		queueID, lookupErr := h.adjacentMusicQueueID(roomID, nullableStringValue(session.CurrentQueueID), req.Action == "next")
		if lookupErr != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to choose queue item")
			return
		}
		_, err = h.DB.Exec(
			`UPDATE music_sessions
			 SET state = CASE WHEN ? = '' THEN state ELSE 'playing' END,
			     current_queue_id = NULLIF(?, ''), follow_user_id = NULL,
			     position_ms = 0, started_at = ?, updated_at = ?
			 WHERE id = ?`,
			queueID, queueID, now, now, session.ID,
		)
	case "set_mode":
		if !allowed(req.Mode, "sequential", "repeat_all", "repeat_one", "shuffle") {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid music mode")
			return
		}
		_, err = h.DB.Exec(`UPDATE music_sessions SET mode = ?, updated_at = ? WHERE id = ?`, req.Mode, now, session.ID)
	case "select_playlist":
		scope := req.PlaylistScope
		if scope == "" {
			scope = "room"
		}
		_, err = h.DB.Exec(
			`UPDATE music_sessions SET playlist_id = ?, playlist_scope = ?, follow_user_id = NULL, updated_at = ? WHERE id = ?`,
			req.PlaylistID, scope, now, session.ID,
		)
	case "follow_user":
		if req.TargetUserID == "" || req.TargetUserID == userID {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "valid target_user_id is required")
			return
		}
		target, targetErr := h.musicSessionForUser(roomID, req.TargetUserID)
		if targetErr != nil || target.State == "stopped" {
			h.jsonError(c, http.StatusNotFound, "not_found", "target user is not listening")
			return
		}
		_, err = h.DB.Exec(
			`UPDATE music_sessions
			 SET state = ?, mode = ?, playlist_id = ?, playlist_scope = ?,
			     current_queue_id = ?, follow_user_id = ?, position_ms = ?,
			     started_at = ?, updated_at = ?
			 WHERE id = ?`,
			target.State, target.Mode, nullableStringValue(target.PlaylistID), nullableStringValue(target.PlaylistScope),
			nullableStringValue(target.CurrentQueueID), req.TargetUserID, target.PositionMS,
			nullableInt64Value(target.StartedAt), now, session.ID,
		)
	case "leave_follow":
		_, err = h.DB.Exec(`UPDATE music_sessions SET follow_user_id = NULL, updated_at = ? WHERE id = ?`, now, session.ID)
	}
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "update music session failed")
		return
	}
	c.JSON(http.StatusOK, h.musicStatePayload(roomID, userID))
}

func (h *Handler) createMusicInvites(c *gin.Context) {
	roomID := c.Param("room_id")
	userID := currentUserID(c)
	if !h.requireMember(c, roomID) {
		return
	}

	var req musicInviteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	session, err := h.musicSessionForUser(roomID, userID)
	if err != nil || session.State == "stopped" {
		h.jsonError(c, http.StatusConflict, "conflict", "start listening before inviting others")
		return
	}

	targets := req.TargetUserIDs
	if req.IncludeAllNotListening {
		targets, err = h.liveUsersNotListening(roomID, userID)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "failed to read live participants")
			return
		}
	}
	if len(targets) == 0 {
		c.JSON(http.StatusOK, gin.H{"invites": []gin.H{}, "music": h.musicStatePayload(roomID, userID)})
		return
	}

	now := nowMillis()
	invites := make([]gin.H, 0, len(targets))
	seen := map[string]bool{}
	for _, targetID := range targets {
		if targetID == "" || targetID == userID || seen[targetID] {
			continue
		}
		seen[targetID] = true
		if !h.isLiveParticipant(roomID, targetID) {
			continue
		}
		id := newID("minv")
		_, err := h.DB.Exec(
			`INSERT INTO music_invites (id, room_id, session_id, inviter_user_id, target_user_id, status, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)`,
			id, roomID, session.ID, userID, targetID, now, now,
		)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "create music invite failed")
			return
		}
		invites = append(invites, gin.H{
			"id": id, "room_id": roomID, "session_id": session.ID, "inviter_user_id": userID,
			"target_user_id": targetID, "status": "pending", "created_at": formatMillis(now),
		})
	}
	c.JSON(http.StatusCreated, gin.H{"invites": invites, "music": h.musicStatePayload(roomID, userID)})
}

func (h *Handler) ensureMusicSession(roomID, userID string) (musicSessionRecord, error) {
	now := nowMillis()
	id := newID("msn")
	_, err := h.DB.Exec(
		`INSERT INTO music_sessions (id, room_id, user_id, state, mode, position_ms, updated_at)
		 VALUES (?, ?, ?, 'stopped', 'sequential', 0, ?)
		 ON CONFLICT(room_id, user_id) DO UPDATE SET updated_at = music_sessions.updated_at`,
		id, roomID, userID, now,
	)
	if err != nil {
		return musicSessionRecord{}, err
	}
	return h.musicSessionForUser(roomID, userID)
}

func (h *Handler) musicSessionForUser(roomID, userID string) (musicSessionRecord, error) {
	row := h.DB.QueryRow(
		`SELECT id, room_id, user_id, state, mode, playlist_id, playlist_scope,
		        current_queue_id, follow_user_id, position_ms, started_at, updated_at
		 FROM music_sessions WHERE room_id = ? AND user_id = ?`,
		roomID, userID,
	)
	return scanMusicSession(row)
}

func scanMusicSession(row scanner) (musicSessionRecord, error) {
	var rec musicSessionRecord
	err := row.Scan(
		&rec.ID, &rec.RoomID, &rec.UserID, &rec.State, &rec.Mode, &rec.PlaylistID,
		&rec.PlaylistScope, &rec.CurrentQueueID, &rec.FollowUserID, &rec.PositionMS,
		&rec.StartedAt, &rec.UpdatedAt,
	)
	return rec, err
}

func (h *Handler) musicListenersPayload(roomID string) []gin.H {
	rows, err := h.DB.Query(
		`SELECT id, room_id, user_id, state, mode, playlist_id, playlist_scope,
		        current_queue_id, follow_user_id, position_ms, started_at, updated_at
		 FROM music_sessions
		 WHERE room_id = ? AND state <> 'stopped'
		 ORDER BY updated_at DESC`,
		roomID,
	)
	if err != nil {
		return []gin.H{}
	}
	defer rows.Close()

	listeners := make([]gin.H, 0)
	for rows.Next() {
		session, err := scanMusicSession(rows)
		if err == nil {
			listeners = append(listeners, h.musicSessionPayload(session))
		}
	}
	return listeners
}

func (h *Handler) nullableMusicSessionPayload(roomID, userID string) any {
	session, err := h.musicSessionForUser(roomID, userID)
	if err != nil {
		return nil
	}
	return h.musicSessionPayload(session)
}

func (h *Handler) musicSessionPayload(session musicSessionRecord) gin.H {
	user, _ := h.userSummary(session.UserID)
	payload := gin.H{
		"id": session.ID, "room_id": session.RoomID, "user": user,
		"state": session.State, "mode": session.Mode,
		"playlist_id": nullableString(session.PlaylistID), "playlist_scope": nullableString(session.PlaylistScope),
		"current_queue_id": nullableString(session.CurrentQueueID), "track": h.musicQueueItemPayload(nullableStringValue(session.CurrentQueueID)),
		"follow_user_id": nullableString(session.FollowUserID), "position_ms": session.PositionMS,
		"started_at": nullableMillis(session.StartedAt), "updated_at": formatMillis(session.UpdatedAt),
	}
	return payload
}

func (h *Handler) musicQueueItemPayload(queueID string) any {
	if queueID == "" {
		return nil
	}
	row := h.DB.QueryRow(
		`SELECT q.id, q.title, q.artist, q.source, q.source_url, q.duration_ms, q.added_by_user_id, q.created_at
		 FROM music_queue q WHERE q.id = ?`,
		queueID,
	)
	var id, title, artist, source, sourceURL, addedBy string
	var duration sql.NullInt64
	var createdAt int64
	if err := row.Scan(&id, &title, &artist, &source, &sourceURL, &duration, &addedBy, &createdAt); err != nil {
		return nil
	}
	return gin.H{
		"id": id, "title": title, "artist": artist, "source": source, "source_url": sourceURL,
		"duration_ms": nullableInt64(duration), "added_by_user_id": addedBy, "created_at": formatMillis(createdAt),
	}
}

func (h *Handler) firstMusicQueueID(roomID string) (string, error) {
	var id string
	err := h.DB.QueryRow(
		`SELECT id FROM music_queue WHERE room_id = ? ORDER BY sort_order ASC, created_at ASC LIMIT 1`,
		roomID,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (h *Handler) adjacentMusicQueueID(roomID, currentID string, next bool) (string, error) {
	rows, err := h.DB.Query(`SELECT id FROM music_queue WHERE room_id = ? ORDER BY sort_order ASC, created_at ASC`, roomID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return "", nil
	}
	if currentID == "" {
		return ids[0], nil
	}
	for i, id := range ids {
		if id != currentID {
			continue
		}
		if next {
			return ids[(i+1)%len(ids)], nil
		}
		return ids[(i-1+len(ids))%len(ids)], nil
	}
	return ids[0], nil
}

func (h *Handler) roomOwnsQueueItem(roomID, queueID string) bool {
	var count int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM music_queue WHERE room_id = ? AND id = ?`, roomID, queueID).Scan(&count)
	return count > 0
}

func (h *Handler) liveUsersNotListening(roomID, excludeUserID string) ([]string, error) {
	rows, err := h.DB.Query(
		`SELECT lp.user_id
		 FROM live_participants lp
		 LEFT JOIN music_sessions ms
		   ON ms.room_id = lp.room_id AND ms.user_id = lp.user_id AND ms.state <> 'stopped'
		 WHERE lp.room_id = ? AND lp.user_id <> ? AND ms.id IS NULL
		 ORDER BY lp.joined_at ASC`,
		roomID, excludeUserID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := make([]string, 0)
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, err
		}
		users = append(users, userID)
	}
	return users, nil
}

func (h *Handler) isLiveParticipant(roomID, userID string) bool {
	var count int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM live_participants WHERE room_id = ? AND user_id = ?`, roomID, userID).Scan(&count)
	return count > 0
}

func (h *Handler) annotateMusicParticipants(roomID string, participants []liveParticipant) []liveParticipant {
	if len(participants) == 0 {
		return participants
	}
	rows, err := h.DB.Query(
		`SELECT user_id, id, follow_user_id FROM music_sessions WHERE room_id = ? AND state <> 'stopped'`,
		roomID,
	)
	if err != nil {
		return participants
	}
	defer rows.Close()

	type status struct {
		sessionID    string
		followUserID sql.NullString
	}
	statuses := map[string]status{}
	for rows.Next() {
		var userID, sessionID string
		var followUserID sql.NullString
		if err := rows.Scan(&userID, &sessionID, &followUserID); err == nil {
			statuses[userID] = status{sessionID: sessionID, followUserID: followUserID}
		}
	}
	for i := range participants {
		item, ok := statuses[participants[i].User.ID]
		if !ok {
			continue
		}
		participants[i].MusicListening = true
		sessionID := item.sessionID
		participants[i].MusicSessionID = &sessionID
		participants[i].ListeningWithUserID = nullableString(item.followUserID)
	}
	return participants
}

func nullableStringValue(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func nullableInt64Value(value sql.NullInt64) any {
	if !value.Valid {
		return nil
	}
	return value.Int64
}
