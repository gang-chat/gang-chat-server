package auth

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/idgen"
)

type playlistRequest struct {
	Name string `json:"name"`
}

type playlistTrackRequest struct {
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Source     string `json:"source"`
	SourceURL  string `json:"source_url"`
	DurationMS *int64 `json:"duration_ms"`
	SortOrder  *int   `json:"sort_order"`
}

func (h *Handler) listPersonalPlaylists(c *gin.Context) {
	playlists, err := h.queryPlaylists("personal", getUserID(c), "")
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "list playlists failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"playlists": playlists})
}

func (h *Handler) createPersonalPlaylist(c *gin.Context) {
	var req playlistRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		errorJSON(c, http.StatusBadRequest, "validation_failed", "playlist name is required")
		return
	}
	now := time.Now().UnixMilli()
	id := idgen.New("ply")
	_, err := h.DB.Exec(
		`INSERT INTO playlists (id, owner_user_id, scope, name, created_at, updated_at) VALUES (?, ?, 'personal', ?, ?, ?)`,
		id, getUserID(c), strings.TrimSpace(req.Name), now, now,
	)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "create playlist failed")
		return
	}
	playlist, err := h.playlistByID(id, getUserID(c), "")
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "read playlist failed")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"playlist": playlist})
}

func (h *Handler) updatePersonalPlaylist(c *gin.Context) {
	var req playlistRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		errorJSON(c, http.StatusBadRequest, "validation_failed", "playlist name is required")
		return
	}
	res, err := h.DB.Exec(
		`UPDATE playlists SET name = ?, updated_at = ? WHERE id = ? AND owner_user_id = ? AND scope = 'personal'`,
		strings.TrimSpace(req.Name), time.Now().UnixMilli(), c.Param("playlist_id"), getUserID(c),
	)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "update playlist failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		errorJSON(c, http.StatusNotFound, "not_found", "playlist not found")
		return
	}
	playlist, _ := h.playlistByID(c.Param("playlist_id"), getUserID(c), "")
	c.JSON(http.StatusOK, gin.H{"playlist": playlist})
}

func (h *Handler) deletePersonalPlaylist(c *gin.Context) {
	_, err := h.DB.Exec(`DELETE FROM playlists WHERE id = ? AND owner_user_id = ? AND scope = 'personal'`, c.Param("playlist_id"), getUserID(c))
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "delete playlist failed")
		return
	}
	c.JSON(http.StatusOK, MessageResponse{OK: true})
}

func (h *Handler) addPersonalPlaylistTrack(c *gin.Context) {
	h.upsertPersonalPlaylistTrack(c, true)
}

func (h *Handler) updatePersonalPlaylistTrack(c *gin.Context) {
	h.upsertPersonalPlaylistTrack(c, false)
}

func (h *Handler) deletePersonalPlaylistTrack(c *gin.Context) {
	if !h.ownsPlaylist(c.Param("playlist_id"), getUserID(c)) {
		errorJSON(c, http.StatusNotFound, "not_found", "playlist not found")
		return
	}
	_, err := h.DB.Exec(`DELETE FROM playlist_tracks WHERE id = ? AND playlist_id = ?`, c.Param("track_id"), c.Param("playlist_id"))
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "delete track failed")
		return
	}
	c.JSON(http.StatusOK, MessageResponse{OK: true})
}

func (h *Handler) upsertPersonalPlaylistTrack(c *gin.Context, create bool) {
	if !h.ownsPlaylist(c.Param("playlist_id"), getUserID(c)) {
		errorJSON(c, http.StatusNotFound, "not_found", "playlist not found")
		return
	}
	var req playlistTrackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errorJSON(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	title := strings.TrimSpace(req.Title)
	sourceURL := strings.TrimSpace(req.SourceURL)
	if title == "" || sourceURL == "" {
		errorJSON(c, http.StatusBadRequest, "validation_failed", "title and source_url are required")
		return
	}
	artist := strings.TrimSpace(req.Artist)
	source := strings.TrimSpace(req.Source)
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
	now := time.Now().UnixMilli()
	if create {
		id := idgen.New("trk")
		_, err := h.DB.Exec(
			`INSERT INTO playlist_tracks (id, playlist_id, title, artist, source, source_url, duration_ms, sort_order, added_by_user_id, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, c.Param("playlist_id"), title, artist, source, sourceURL, duration, sortOrder, getUserID(c), now, now,
		)
		if err != nil {
			errorJSON(c, http.StatusInternalServerError, "internal_error", "add track failed")
			return
		}
		track, _ := h.trackByID(id)
		c.JSON(http.StatusCreated, gin.H{"track": track})
		return
	}
	res, err := h.DB.Exec(
		`UPDATE playlist_tracks SET title = ?, artist = ?, source = ?, source_url = ?, duration_ms = ?, sort_order = ?, updated_at = ?
		 WHERE id = ? AND playlist_id = ?`,
		title, artist, source, sourceURL, duration, sortOrder, now, c.Param("track_id"), c.Param("playlist_id"),
	)
	if err != nil {
		errorJSON(c, http.StatusInternalServerError, "internal_error", "update track failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		errorJSON(c, http.StatusNotFound, "not_found", "track not found")
		return
	}
	track, _ := h.trackByID(c.Param("track_id"))
	c.JSON(http.StatusOK, gin.H{"track": track})
}

func (h *Handler) queryPlaylists(scope, ownerUserID, roomID string) ([]gin.H, error) {
	var rows *sql.Rows
	var err error
	if scope == "personal" {
		rows, err = h.DB.Query(
			`SELECT p.id, p.scope, p.name, p.updated_at, COUNT(t.id)
			 FROM playlists p LEFT JOIN playlist_tracks t ON t.playlist_id = p.id
			 WHERE p.scope = 'personal' AND p.owner_user_id = ?
			 GROUP BY p.id ORDER BY p.updated_at DESC`,
			ownerUserID,
		)
	} else {
		rows, err = h.DB.Query(
			`SELECT p.id, p.scope, p.name, p.updated_at, COUNT(t.id)
			 FROM playlists p LEFT JOIN playlist_tracks t ON t.playlist_id = p.id
			 WHERE p.scope = 'room' AND p.room_id = ?
			 GROUP BY p.id ORDER BY p.updated_at DESC`,
			roomID,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	playlists := make([]gin.H, 0)
	for rows.Next() {
		var id, playlistScope, name string
		var updatedAt int64
		var count int
		if err := rows.Scan(&id, &playlistScope, &name, &updatedAt, &count); err != nil {
			return nil, err
		}
		playlists = append(playlists, gin.H{"id": id, "scope": playlistScope, "name": name, "track_count": count, "updated_at": rfc3339(time.UnixMilli(updatedAt))})
	}
	return playlists, nil
}

func (h *Handler) playlistByID(id, ownerUserID, roomID string) (gin.H, error) {
	var playlistID, scope, name string
	var updatedAt int64
	var count int
	query := `SELECT p.id, p.scope, p.name, p.updated_at, COUNT(t.id)
	 FROM playlists p LEFT JOIN playlist_tracks t ON t.playlist_id = p.id
	 WHERE p.id = ? AND ((p.scope = 'personal' AND p.owner_user_id = ?) OR (p.scope = 'room' AND p.room_id = ?))
	 GROUP BY p.id`
	err := h.DB.QueryRow(query, id, ownerUserID, roomID).Scan(&playlistID, &scope, &name, &updatedAt, &count)
	return gin.H{"id": playlistID, "scope": scope, "name": name, "track_count": count, "updated_at": rfc3339(time.UnixMilli(updatedAt))}, err
}

func (h *Handler) ownsPlaylist(playlistID, userID string) bool {
	var exists int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM playlists WHERE id = ? AND owner_user_id = ? AND scope = 'personal'`, playlistID, userID).Scan(&exists)
	return exists > 0
}

func (h *Handler) trackByID(trackID string) (gin.H, error) {
	var id, title, artist, source, sourceURL string
	var duration sql.NullInt64
	var sortOrder int
	err := h.DB.QueryRow(
		`SELECT id, title, artist, source, source_url, duration_ms, sort_order FROM playlist_tracks WHERE id = ?`,
		trackID,
	).Scan(&id, &title, &artist, &source, &sourceURL, &duration, &sortOrder)
	var durationValue any
	if duration.Valid {
		durationValue = duration.Int64
	}
	return gin.H{"id": id, "title": title, "artist": artist, "source": source, "source_url": sourceURL, "duration_ms": durationValue, "sort_order": sortOrder}, err
}
