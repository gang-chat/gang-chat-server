package chat

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (h *Handler) getMusicState(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	c.JSON(http.StatusOK, h.musicStatePayload(roomID, currentUserID(c)))
}

func (h *Handler) controlMusicPlayback(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	var req musicPlaybackRequest
	if err := c.ShouldBindJSON(&req); err != nil || !allowed(req.Action, "play", "pause", "previous", "next", "set_mode", "select_playlist") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid music action")
		return
	}
	now := nowMillis()
	_, _ = h.DB.Exec(`INSERT OR IGNORE INTO music_playback (room_id, state, mode, updated_at) VALUES (?, 'stopped', 'sequential', ?)`, roomID, now)
	switch req.Action {
	case "play":
		_, _ = h.DB.Exec(`UPDATE music_playback SET state = 'playing', updated_at = ? WHERE room_id = ?`, now, roomID)
	case "pause":
		_, _ = h.DB.Exec(`UPDATE music_playback SET state = 'paused', updated_at = ? WHERE room_id = ?`, now, roomID)
	case "set_mode":
		if !allowed(req.Mode, "sequential", "repeat_all", "repeat_one", "shuffle") {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid music mode")
			return
		}
		_, _ = h.DB.Exec(`UPDATE music_playback SET mode = ?, updated_at = ? WHERE room_id = ?`, req.Mode, now, roomID)
	case "select_playlist":
		scope := req.PlaylistScope
		if scope == "" {
			scope = "room"
		}
		_, _ = h.DB.Exec(`UPDATE music_playback SET playlist_id = ?, playlist_scope = ?, updated_at = ? WHERE room_id = ?`, req.PlaylistID, scope, now, roomID)
	case "next", "previous":
		_, _ = h.DB.Exec(`UPDATE music_playback SET updated_at = ? WHERE room_id = ?`, now, roomID)
	}
	c.JSON(http.StatusOK, h.musicStatePayload(roomID, currentUserID(c)))
}

func (h *Handler) addMusicQueue(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	var req musicQueueRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.SourceURL) == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "title and source_url are required")
		return
	}
	var duration any
	if req.DurationMS != nil {
		duration = *req.DurationMS
	}
	_, err := h.DB.Exec(
		`INSERT INTO music_queue (id, room_id, title, artist, source_url, duration_ms, added_by_user_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		newID("mq"), roomID, strings.TrimSpace(req.Title), strings.TrimSpace(req.Artist), strings.TrimSpace(req.SourceURL), duration, currentUserID(c), nowMillis(),
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "add music queue failed")
		return
	}
	c.JSON(http.StatusCreated, h.musicStatePayload(roomID, currentUserID(c)))
}

func (h *Handler) listRoomPlaylists(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	playlists, err := h.queryPlaylists(roomID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "list playlists failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"playlists": playlists})
}

func (h *Handler) createRoomPlaylist(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.isAdmin(roomID, currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	var req playlistRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "playlist name is required")
		return
	}
	id := newID("ply")
	now := nowMillis()
	_, err := h.DB.Exec(`INSERT INTO playlists (id, room_id, scope, name, created_at, updated_at) VALUES (?, ?, 'room', ?, ?, ?)`, id, roomID, strings.TrimSpace(req.Name), now, now)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "create playlist failed")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"playlist": h.playlistPayload(id)})
}

func (h *Handler) updateRoomPlaylist(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.isAdmin(roomID, currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	var req playlistRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "playlist name is required")
		return
	}
	_, _ = h.DB.Exec(`UPDATE playlists SET name = ?, updated_at = ? WHERE id = ? AND room_id = ? AND scope = 'room'`, strings.TrimSpace(req.Name), nowMillis(), c.Param("playlist_id"), roomID)
	c.JSON(http.StatusOK, gin.H{"playlist": h.playlistPayload(c.Param("playlist_id"))})
}

func (h *Handler) deleteRoomPlaylist(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.isAdmin(roomID, currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	_, _ = h.DB.Exec(`DELETE FROM playlists WHERE id = ? AND room_id = ? AND scope = 'room'`, c.Param("playlist_id"), roomID)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) addRoomPlaylistTrack(c *gin.Context) {
	h.upsertRoomPlaylistTrack(c, true)
}

func (h *Handler) updateRoomPlaylistTrack(c *gin.Context) {
	h.upsertRoomPlaylistTrack(c, false)
}

func (h *Handler) deleteRoomPlaylistTrack(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.isAdmin(roomID, currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	if !h.roomOwnsPlaylist(roomID, c.Param("playlist_id")) {
		h.jsonError(c, http.StatusNotFound, "not_found", "playlist not found")
		return
	}
	_, _ = h.DB.Exec(`DELETE FROM playlist_tracks WHERE id = ? AND playlist_id = ?`, c.Param("track_id"), c.Param("playlist_id"))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) upsertRoomPlaylistTrack(c *gin.Context, create bool) {
	roomID := c.Param("room_id")
	if !h.isAdmin(roomID, currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	if !h.roomOwnsPlaylist(roomID, c.Param("playlist_id")) {
		h.jsonError(c, http.StatusNotFound, "not_found", "playlist not found")
		return
	}
	var req playlistTrackRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.SourceURL) == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "title and source_url are required")
		return
	}
	source := req.Source
	if source == "" {
		source = "manual_url"
	}
	sortOrder := 10
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	var duration any
	if req.DurationMS != nil {
		duration = *req.DurationMS
	}
	now := nowMillis()
	if create {
		id := newID("trk")
		_, err := h.DB.Exec(
			`INSERT INTO playlist_tracks (id, playlist_id, title, artist, source, source_url, duration_ms, sort_order, added_by_user_id, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, c.Param("playlist_id"), strings.TrimSpace(req.Title), strings.TrimSpace(req.Artist), source, strings.TrimSpace(req.SourceURL), duration, sortOrder, currentUserID(c), now, now,
		)
		if err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "add track failed")
			return
		}
		c.JSON(http.StatusCreated, gin.H{"track": h.trackPayload(id)})
		return
	}
	_, _ = h.DB.Exec(
		`UPDATE playlist_tracks SET title = ?, artist = ?, source = ?, source_url = ?, duration_ms = ?, sort_order = ?, updated_at = ?
		 WHERE id = ? AND playlist_id = ?`,
		strings.TrimSpace(req.Title), strings.TrimSpace(req.Artist), source, strings.TrimSpace(req.SourceURL), duration, sortOrder, now, c.Param("track_id"), c.Param("playlist_id"),
	)
	c.JSON(http.StatusOK, gin.H{"track": h.trackPayload(c.Param("track_id"))})
}

func (h *Handler) musicStatePayload(roomID string, userIDs ...string) gin.H {
	now := nowMillis()
	_, _ = h.DB.Exec(`INSERT OR IGNORE INTO music_playback (room_id, state, mode, updated_at) VALUES (?, 'stopped', 'sequential', ?)`, roomID, now)
	var state, mode string
	var playlistID, playlistScope, trackID sql.NullString
	var position, updatedAt int64
	_ = h.DB.QueryRow(`SELECT state, mode, playlist_id, playlist_scope, track_id, position_ms, updated_at FROM music_playback WHERE room_id = ?`, roomID).
		Scan(&state, &mode, &playlistID, &playlistScope, &trackID, &position, &updatedAt)
	payload := gin.H{
		"playback": gin.H{
			"state": state, "mode": mode, "playlist_id": nullableString(playlistID),
			"playlist_scope": nullableString(playlistScope), "track": nil, "position_ms": position,
			"updated_at": formatMillis(updatedAt),
		},
		"queue":     h.musicQueuePayload(roomID),
		"listeners": h.musicListenersPayload(roomID),
	}
	if len(userIDs) > 0 && userIDs[0] != "" {
		payload["my_session"] = h.nullableMusicSessionPayload(roomID, userIDs[0])
	}
	return payload
}

func (h *Handler) musicQueuePayload(roomID string) []gin.H {
	rows, _ := h.DB.Query(
		`SELECT q.id, q.title, q.artist, q.source, q.source_url, q.duration_ms, q.added_by_user_id, q.created_at
		 FROM music_queue q WHERE q.room_id = ? ORDER BY q.sort_order ASC, q.created_at ASC`,
		roomID,
	)
	queue := make([]gin.H, 0)
	if rows == nil {
		return queue
	}
	defer rows.Close()
	for rows.Next() {
		var id, title, artist, source, sourceURL, addedBy string
		var duration sql.NullInt64
		var createdAt int64
		if err := rows.Scan(&id, &title, &artist, &source, &sourceURL, &duration, &addedBy, &createdAt); err == nil {
			queue = append(queue, gin.H{"id": id, "title": title, "artist": artist, "source": source, "source_url": sourceURL, "duration_ms": nullableInt64(duration), "added_by_user_id": addedBy, "created_at": formatMillis(createdAt)})
		}
	}
	return queue
}

func (h *Handler) queryPlaylists(roomID string) ([]gin.H, error) {
	rows, err := h.DB.Query(
		`SELECT p.id, p.scope, p.name, p.updated_at, COUNT(t.id)
		 FROM playlists p LEFT JOIN playlist_tracks t ON t.playlist_id = p.id
		 WHERE p.scope = 'room' AND p.room_id = ?
		 GROUP BY p.id ORDER BY p.updated_at DESC`,
		roomID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	playlists := make([]gin.H, 0)
	for rows.Next() {
		var id, scope, name string
		var updatedAt int64
		var count int
		if err := rows.Scan(&id, &scope, &name, &updatedAt, &count); err != nil {
			return nil, err
		}
		playlists = append(playlists, gin.H{"id": id, "scope": scope, "name": name, "track_count": count, "updated_at": formatMillis(updatedAt)})
	}
	return playlists, nil
}

func (h *Handler) playlistPayload(id string) gin.H {
	var scope, name string
	var updatedAt int64
	var count int
	_ = h.DB.QueryRow(
		`SELECT p.scope, p.name, p.updated_at, COUNT(t.id)
		 FROM playlists p LEFT JOIN playlist_tracks t ON t.playlist_id = p.id
		 WHERE p.id = ? GROUP BY p.id`,
		id,
	).Scan(&scope, &name, &updatedAt, &count)
	return gin.H{"id": id, "scope": scope, "name": name, "track_count": count, "updated_at": formatMillis(updatedAt)}
}

func (h *Handler) roomOwnsPlaylist(roomID, playlistID string) bool {
	var exists int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM playlists WHERE id = ? AND room_id = ? AND scope = 'room'`, playlistID, roomID).Scan(&exists)
	return exists > 0
}

func (h *Handler) trackPayload(id string) gin.H {
	var title, artist, source, sourceURL string
	var duration sql.NullInt64
	var sortOrder int
	_ = h.DB.QueryRow(`SELECT title, artist, source, source_url, duration_ms, sort_order FROM playlist_tracks WHERE id = ?`, id).
		Scan(&title, &artist, &source, &sourceURL, &duration, &sortOrder)
	return gin.H{"id": id, "title": title, "artist": artist, "source": source, "source_url": sourceURL, "duration_ms": nullableInt64(duration), "sort_order": sortOrder}
}
