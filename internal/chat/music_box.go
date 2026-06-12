package chat

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/eventbus"
	"github.com/zhuangkaiyi/gang-chat/server/internal/musicbox"
)

// The music box is a room-level, server-side player: the server downloads and
// transcodes tracks to Opus and broadcasts a single audio track into the
// room's LiveKit session via a bot participant. See internal/musicbox.
//
// Permissions (per product decision): any room member can search, enqueue, and
// control playback (play/pause/resume/skip/stop). Removing a queue item is
// allowed for the member who added it or any room admin.

func (h *Handler) musicBoxReady(c *gin.Context) bool {
	if h.MusicBox == nil || !h.MusicBox.Enabled() {
		h.jsonError(c, http.StatusServiceUnavailable, "music_box_unavailable", "music box is not available")
		return false
	}
	return true
}

func (h *Handler) searchMusicBox(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireMember(c, roomID) {
		return
	}
	if !h.musicBoxReady(c) {
		return
	}
	keyword := strings.TrimSpace(c.Query("keyword"))
	if keyword == "" {
		keyword = strings.TrimSpace(c.Query("name"))
	}
	if keyword == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "keyword is required")
		return
	}
	count, _ := strconv.Atoi(c.Query("count"))
	page, _ := strconv.Atoi(c.Query("page"))
	results, err := h.MusicBox.Router().Search(c.Request.Context(), c.Query("source"), keyword, count, page)
	if err != nil {
		h.jsonError(c, http.StatusBadGateway, "upstream_error", "music search failed: "+err.Error())
		return
	}
	tracks := make([]gin.H, 0, len(results))
	for _, r := range results {
		tracks = append(tracks, gin.H{
			"track_id": r.ID,
			"name":     r.Name,
			"artists":  r.Artists,
			"source":   r.Source,
		})
	}
	c.JSON(http.StatusOK, gin.H{"results": tracks})
}

func (h *Handler) getMusicBoxState(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	if h.MusicBox == nil {
		h.jsonError(c, http.StatusServiceUnavailable, "music_box_unavailable", "music box is not available")
		return
	}
	c.JSON(http.StatusOK, h.musicBoxStatePayload(roomID))
}

func (h *Handler) enqueueMusicBox(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireMember(c, roomID) {
		return
	}
	if !h.musicBoxReady(c) {
		return
	}
	var req musicBoxEnqueueRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.TrackID) == "" || strings.TrimSpace(req.Title) == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "track_id and title are required")
		return
	}
	var duration int64
	if req.DurationMS != nil {
		duration = *req.DurationMS
	}
	_, err := h.MusicBox.Enqueue(c.Request.Context(), musicbox.EnqueueParams{
		RoomID:        roomID,
		Source:        strings.TrimSpace(req.Source),
		TrackID:       strings.TrimSpace(req.TrackID),
		Title:         strings.TrimSpace(req.Title),
		Artist:        strings.TrimSpace(req.Artist),
		DurationMS:    duration,
		AddedByUserID: currentUserID(c),
	})
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "enqueue failed")
		return
	}
	c.JSON(http.StatusCreated, h.musicBoxStatePayload(roomID))
}

func (h *Handler) removeMusicBoxItem(c *gin.Context) {
	roomID := c.Param("room_id")
	itemID := c.Param("item_id")
	if !h.requireMember(c, roomID) {
		return
	}
	if !h.musicBoxReady(c) {
		return
	}
	// Any room member may remove a queued track, same as enqueue and playback
	// control. Restricting removal to the adder or an admin left tracks stuck in
	// the queue when the adder had left the room and no admin was around.
	if err := h.MusicBox.RemoveItem(roomID, itemID); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "remove failed")
		return
	}
	c.JSON(http.StatusOK, h.musicBoxStatePayload(roomID))
}

func (h *Handler) controlMusicBox(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireMember(c, roomID) {
		return
	}
	if !h.musicBoxReady(c) {
		return
	}
	var req musicBoxControlRequest
	if err := c.ShouldBindJSON(&req); err != nil || !allowed(req.Action, "play", "pause", "resume", "skip", "next", "stop") {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid music box action")
		return
	}
	if err := h.MusicBox.Control(roomID, req.Action); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "control failed: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, h.musicBoxStatePayload(roomID))
}

// publishMusicBoxSnapshot fans out a fresh music box state to a room's SSE
// subscribers. Best-effort: a nil bus is swallowed.
func (h *Handler) publishMusicBoxSnapshot(roomID string) {
	if h == nil || h.Bus == nil || roomID == "" {
		return
	}
	h.Bus.PublishRoom(roomID, eventbus.Event{
		Type:   "music_box_changed",
		RoomID: roomID,
		Data:   h.musicBoxStatePayload(roomID),
	})
}

// musicBoxStatePayload builds the SSE/HTTP snapshot for a room's music box.
func (h *Handler) musicBoxStatePayload(roomID string) gin.H {
	if h.MusicBox == nil {
		return gin.H{"enabled": false}
	}
	st, items, err := h.MusicBox.State(roomID)
	if err != nil {
		return gin.H{"enabled": h.MusicBox.Enabled()}
	}
	used, capBytes := h.MusicBox.RoomUsage(roomID)
	queue := make([]gin.H, 0, len(items))
	for _, it := range items {
		queue = append(queue, gin.H{
			"id":               it.ID,
			"source":           it.Source,
			"track_id":         it.TrackID,
			"title":            it.Title,
			"artist":           it.Artist,
			"duration_ms":      it.DurationMS,
			"status":           string(it.Status),
			"file_size_bytes":  it.FileSizeBytes,
			"error":            it.Error,
			"added_by_user_id": it.AddedByUserID,
			"created_at":       formatMillis(it.CreatedAt),
		})
	}
	return gin.H{
		"enabled": h.MusicBox.Enabled(),
		"playback": gin.H{
			"state":           string(st.State),
			"current_item_id": st.CurrentItemID,
			"position_ms":     st.PositionMS,
			"volume":          st.Volume,
			"updated_at":      formatMillis(st.UpdatedAt),
		},
		"queue": queue,
		"usage": gin.H{"used_bytes": used, "limit_bytes": capBytes},
	}
}
