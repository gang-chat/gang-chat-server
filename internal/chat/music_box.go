package chat

import (
	"database/sql"
	"errors"
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
// Search and enqueue are available to room members. Playback, queue mutation,
// and saved-playlist controls are authorized per command from the caller's
// current room role and the active source owner; client capabilities are only
// a presentation hint and never replace these checks.

const musicBoxRequestQueueDisplayName = "点歌队列"

type musicBoxActorCapabilities struct {
	CanEnqueue    bool
	CanSwitch     bool
	CanControl    bool
	CanChangeMode bool
	CanReorder    bool
	CanClear      bool
	CanPlayNow    bool
	AllowedModes  []string
}

func (h *Handler) musicBoxReady(c *gin.Context) bool {
	if h.MusicBox == nil || !h.MusicBox.Enabled() {
		h.jsonError(c, http.StatusServiceUnavailable, "music_box_unavailable", "music box is not available")
		return false
	}
	return true
}

func (h *Handler) musicBoxPermissionDenied(c *gin.Context) {
	h.jsonError(
		c,
		http.StatusForbidden,
		"music_box_permission_denied",
		"the current room role cannot perform this music box action",
	)
}

func (h *Handler) musicBoxRoleRank(roomID, userID string) int {
	if h == nil || h.DB == nil || strings.TrimSpace(userID) == "" {
		return 0
	}
	if h.isSuperuser(userID) && h.roomIDExists(roomID) {
		return 4
	}
	var role string
	if err := h.DB.QueryRow(
		`SELECT role FROM room_memberships WHERE room_id = ? AND user_id = ?`,
		roomID,
		userID,
	).Scan(&role); err != nil {
		return 0
	}
	return roleRank(role)
}

func (h *Handler) musicBoxCapabilities(
	roomID, actorID string,
	state *musicbox.RoomState,
) musicBoxActorCapabilities {
	actorRank := h.musicBoxRoleRank(roomID, actorID)
	capabilities := musicBoxActorCapabilities{
		CanEnqueue: actorRank > 0,
		CanSwitch:  actorRank > 0,
	}
	if state == nil {
		return capabilities
	}
	capabilities.AllowedModes = []string{"sequential", "repeat_one"}
	if state.ActiveSourceType != musicbox.ActiveSourceTemporary {
		capabilities.AllowedModes = append(
			capabilities.AllowedModes,
			"repeat_all",
			"shuffle",
		)
	}

	switch state.ActiveSourceType {
	case musicbox.ActiveSourceTemporary:
		capabilities.CanControl = actorRank >= roleRank("admin")
	case musicbox.ActiveSourceRoomPlaylist:
		capabilities.CanControl = actorRank > 0
	case musicbox.ActiveSourceUserPlaylist:
		ownerID := strings.TrimSpace(state.ActivePlaylistOwnerID)
		if ownerID != "" && ownerID == actorID {
			capabilities.CanControl = true
		} else if actorRank >= roleRank("admin") && ownerID != "" {
			capabilities.CanControl = actorRank > h.musicBoxRoleRank(roomID, ownerID)
		}
	}
	capabilities.CanChangeMode = capabilities.CanControl
	capabilities.CanReorder = capabilities.CanControl
	capabilities.CanPlayNow = capabilities.CanControl
	capabilities.CanClear = actorRank >= roleRank("admin")
	return capabilities
}

func switchMusicBoxCommandAllowed(
	action string,
	capabilities musicBoxActorCapabilities,
) bool {
	switch action {
	case "play", "pause", "resume", "skip", "next", "previous", "stop":
		return capabilities.CanControl
	case "set_mode":
		return capabilities.CanChangeMode
	case "play_now":
		return capabilities.CanPlayNow
	case "clear_temporary_playlist":
		return capabilities.CanClear
	default:
		return false
	}
}

func (h *Handler) musicBoxCanRemoveItem(
	actorID string,
	item *musicbox.QueueItem,
	capabilities musicBoxActorCapabilities,
) bool {
	if item == nil || strings.TrimSpace(actorID) == "" {
		return false
	}
	if item.QueueScope == musicbox.QueueScopeTemporary {
		return item.AddedByUserID == actorID || capabilities.CanClear
	}
	return capabilities.CanControl
}

func (h *Handler) musicBoxQueueItemPermission(
	roomID, actorID, itemID string,
) (*musicbox.QueueItem, bool, error) {
	state, activeItems, err := h.MusicBox.State(roomID)
	if err != nil {
		return nil, false, err
	}
	capabilities := h.musicBoxCapabilities(roomID, actorID, state)
	for _, item := range activeItems {
		if item.ID == itemID {
			return item, h.musicBoxCanRemoveItem(actorID, item, capabilities), nil
		}
	}
	temporaryItems, err := h.MusicBox.TemporaryQueue(roomID)
	if err != nil {
		return nil, false, err
	}
	for _, item := range temporaryItems {
		if item.ID == itemID {
			return item, h.musicBoxCanRemoveItem(actorID, item, capabilities), nil
		}
	}
	return nil, false, nil
}

func (h *Handler) searchMusicBox(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
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
	results, err := h.MusicBox.Search(c.Request.Context(), c.Query("source"), keyword, count, page)
	if err != nil {
		h.jsonError(c, http.StatusBadGateway, "upstream_error", "music search failed: "+err.Error())
		return
	}
	tracks := make([]gin.H, 0, len(results))
	for _, r := range results {
		tracks = append(tracks, gin.H{
			"track_id": r.TrackID,
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
	c.JSON(http.StatusOK, h.musicBoxStatePayload(roomID, currentUserID(c)))
}

func (h *Handler) enqueueMusicBox(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
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
		if errors.Is(err, musicbox.ErrQueueItemAlreadyExists) {
			h.jsonError(c, http.StatusConflict, "music_box_item_already_queued", "music box item is already queued")
			return
		}
		if errors.Is(err, musicbox.ErrQueueLimitReached) {
			h.jsonError(c, http.StatusConflict, "music_box_queue_limit_reached", "music box request queue reached its 200 item limit")
			return
		}
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "enqueue failed")
		return
	}
	c.JSON(http.StatusCreated, h.musicBoxStatePayload(roomID, currentUserID(c)))
}

func (h *Handler) removeMusicBoxItem(c *gin.Context) {
	roomID := c.Param("room_id")
	itemID := c.Param("item_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	if !h.musicBoxReady(c) {
		return
	}
	actorID := currentUserID(c)
	item, canRemove, err := h.musicBoxQueueItemPermission(roomID, actorID, itemID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "load music box item failed")
		return
	}
	if item == nil {
		h.jsonError(c, http.StatusNotFound, "not_found", "music box queue item not found")
		return
	}
	if !canRemove {
		h.musicBoxPermissionDenied(c)
		return
	}
	if err := h.MusicBox.RemoveItem(roomID, itemID); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "remove failed")
		return
	}
	c.JSON(http.StatusOK, h.musicBoxStatePayload(roomID, actorID))
}

func (h *Handler) controlMusicBox(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	if !h.musicBoxReady(c) {
		return
	}
	var req musicBoxControlRequest
	if err := c.ShouldBindJSON(&req); err != nil || !allowed(
		req.Action,
		"play", "pause", "resume", "skip", "next", "previous", "play_now", "set_mode", "stop", "clear_temporary_playlist",
	) {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid music box action")
		return
	}
	if len(strings.TrimSpace(req.CommandID)) > 128 {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "command_id is too long")
		return
	}
	if req.Action == "set_mode" && !allowed(
		req.Mode,
		"sequential", "repeat_one", "repeat_all", "shuffle",
	) {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid playback mode")
		return
	}
	if req.Action == "play_now" && strings.TrimSpace(req.ItemID) == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "item_id is required")
		return
	}
	actorID := currentUserID(c)
	st, _, err := h.MusicBox.State(roomID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "load music box state failed")
		return
	}
	capabilities := h.musicBoxCapabilities(roomID, actorID, st)
	allowedCommand := switchMusicBoxCommandAllowed(req.Action, capabilities)
	if !allowedCommand {
		h.musicBoxPermissionDenied(c)
		return
	}
	if err := h.MusicBox.ApplyItemControl(
		roomID,
		req.Action,
		strings.TrimSpace(req.ItemID),
		strings.TrimSpace(req.Mode),
		strings.TrimSpace(req.CommandID),
		req.ExpectedRevision,
	); err != nil {
		if errors.Is(err, musicbox.ErrRevisionConflict) {
			c.JSON(http.StatusConflict, gin.H{
				"error": gin.H{
					"code":    "music_box_revision_conflict",
					"message": "music box state changed; refresh and try again",
				},
				"state": h.musicBoxStatePayload(roomID, actorID),
			})
			return
		}
		if errors.Is(err, musicbox.ErrQueueItemNotFound) {
			h.jsonError(c, http.StatusNotFound, "not_found", "music box queue item not found")
			return
		}
		if errors.Is(err, musicbox.ErrQueueItemNotReady) {
			h.jsonError(c, http.StatusConflict, "music_box_item_not_ready", "music box queue item is not ready")
			return
		}
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "control failed: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, h.musicBoxStatePayload(roomID, actorID))
}

func (h *Handler) activateMusicBoxPlaylist(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) || !h.musicBoxReady(c) {
		return
	}
	var req musicBoxActivatePlaylistRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	sourceType := strings.TrimSpace(req.SourceType)
	playlistID := strings.TrimSpace(req.PlaylistID)
	startItemID := strings.TrimSpace(req.StartItemID)
	if !allowed(sourceType, "temporary", "room_playlist", "user_playlist") ||
		(sourceType != "temporary" && playlistID == "") ||
		(startItemID != "" && !req.StartPlay) {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid music box source")
		return
	}

	actorID := currentUserID(c)
	if sourceType == "temporary" {
		if req.StartPlay && !h.isAdmin(roomID, actorID) {
			h.musicBoxPermissionDenied(c)
			return
		}
		if err := h.MusicBox.ActivatePlaylist(
			roomID,
			musicbox.ActiveSourceTemporary,
			"",
			musicBoxRequestQueueDisplayName,
			"",
			0,
			actorID,
			nil,
			req.StartPlay,
			startItemID,
			-1,
		); err != nil {
			if errors.Is(err, musicbox.ErrQueueItemNotFound) {
				h.jsonError(c, http.StatusNotFound, "not_found", "music box queue item not found")
				return
			}
			if errors.Is(err, musicbox.ErrQueueItemNotReady) {
				h.jsonError(c, http.StatusConflict, "music_box_item_not_ready", "music box queue item is not ready")
				return
			}
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "switch music box source failed")
			return
		}
		c.JSON(http.StatusOK, h.musicBoxStatePayload(roomID, actorID))
		return
	}

	page := 1
	tracks := make([]musicbox.SnapshotTrack, 0)
	startTrackIndex := -1
	playlistName := ""
	var playlistCreatedAt int64
	for {
		var result musicbox.PlaylistItemsPage
		var err error
		if sourceType == "room_playlist" {
			result, err = h.Playlists.RoomPlaylistItems(
				c.Request.Context(), roomID, playlistID, "", "", page, musicbox.MaxPlaylistPage,
			)
		} else {
			result, err = h.Playlists.UserPlaylistItems(
				c.Request.Context(), actorID, playlistID, "", "", page, musicbox.MaxPlaylistPage,
			)
		}
		if err != nil {
			if errors.Is(err, musicbox.ErrPlaylistNotFound) {
				h.jsonError(c, http.StatusNotFound, "not_found", "music playlist not found")
			} else {
				h.jsonError(c, http.StatusInternalServerError, "internal_error", "load music playlist failed")
			}
			return
		}
		playlistName = result.Playlist.Name
		playlistCreatedAt = result.Playlist.CreatedAt
		for _, item := range result.Items {
			if startItemID != "" && item.ID == startItemID {
				startTrackIndex = len(tracks)
			}
			tracks = append(tracks, musicbox.SnapshotTrack{
				Source: item.Source, TrackID: item.ExternalTrackID,
				Title: item.Title, Artist: strings.Join(item.Artists, "、"),
				DurationMS: item.DurationMS,
			})
		}
		if !result.HasMore {
			break
		}
		page++
	}
	if startItemID != "" {
		if startTrackIndex < 0 {
			h.jsonError(c, http.StatusNotFound, "not_found", "music playlist item not found")
			return
		}
	}
	activeType := musicbox.ActiveSourceRoomPlaylist
	ownerID := ""
	if sourceType == "user_playlist" {
		activeType = musicbox.ActiveSourceUserPlaylist
		ownerID = actorID
	}
	if err := h.MusicBox.ActivatePlaylist(
		roomID, activeType, playlistID, playlistName, ownerID,
		playlistCreatedAt, actorID, tracks, req.StartPlay, "", startTrackIndex,
	); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "activate music playlist failed")
		return
	}
	c.JSON(http.StatusOK, h.musicBoxStatePayload(roomID, actorID))
}

func (h *Handler) cloneActiveMusicBoxPlaylist(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) || !h.musicBoxReady(c) {
		return
	}
	var req musicBoxCloneActivePlaylistRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	req.PlaylistID = strings.TrimSpace(req.PlaylistID)
	req.SnapshotID = strings.TrimSpace(req.SnapshotID)
	if req.PlaylistID == "" || req.SnapshotID == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "playlist snapshot is required")
		return
	}

	st, items, err := h.MusicBox.State(roomID)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "load active music playlist failed")
		return
	}
	actorID := currentUserID(c)
	if st.ActiveSourceType != musicbox.ActiveSourceUserPlaylist ||
		st.ActivePlaylistOwnerID == "" ||
		st.ActivePlaylistOwnerID == actorID {
		h.jsonError(c, http.StatusConflict, "active_playlist_not_cloneable", "only another user's active playlist can be cloned")
		return
	}
	if st.ActivePlaylistID != req.PlaylistID || st.ActiveSnapshotID != req.SnapshotID {
		h.jsonError(c, http.StatusConflict, "active_playlist_changed", "the active playlist changed; reopen it and try again")
		return
	}

	ownerName := st.ActivePlaylistOwnerID
	if owner := h.musicBoxRequesterPayloads(roomID, nil, st.ActivePlaylistOwnerID)[st.ActivePlaylistOwnerID]; owner != nil {
		if displayName, ok := owner["display_name"].(string); ok && strings.TrimSpace(displayName) != "" {
			ownerName = strings.TrimSpace(displayName)
		}
	}
	tracks := make([]musicbox.SnapshotTrack, 0, len(items))
	for _, item := range items {
		tracks = append(tracks, musicbox.SnapshotTrack{
			Source: item.Source, TrackID: item.TrackID, Title: item.Title,
			Artist: item.Artist, DurationMS: item.DurationMS,
		})
	}
	playlist, err := h.Playlists.CloneSnapshotToUserPlaylist(
		c.Request.Context(),
		actorID,
		ownerName+"的歌单 · "+st.ActivePlaylistName,
		tracks,
	)
	if err != nil {
		switch {
		case errors.Is(err, musicbox.ErrPlaylistLimit):
			h.jsonError(c, http.StatusConflict, "playlist_limit_reached", "playlist limit reached")
		case errors.Is(err, musicbox.ErrPlaylistItemLimit):
			h.jsonError(c, http.StatusConflict, "playlist_item_limit_reached", "playlist item limit reached")
		default:
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "clone active music playlist failed")
		}
		return
	}
	c.JSON(http.StatusCreated, gin.H{"playlist": musicPlaylistPayload(playlist)})
}

// publishMusicBoxSnapshot fans out a fresh music box state to a room's SSE
// subscribers. Best-effort: a nil bus is swallowed.
func (h *Handler) publishMusicBoxSnapshot(roomID string) {
	if h == nil || h.Bus == nil || roomID == "" {
		return
	}
	if h.MusicBox != nil {
		h.MusicBox.RecordFullSnapshotEvent()
	}
	h.Bus.PublishRoom(roomID, eventbus.Event{
		Type:   "music_box_changed",
		RoomID: roomID,
		// Room broadcasts are deliberately conservative because subscribers have
		// different roles. Current clients refresh the personalized HTTP snapshot;
		// older clients safely receive disabled capabilities rather than an
		// over-authorized shared snapshot.
		Data: h.musicBoxStatePayload(roomID, ""),
	})
}

// publishMusicBoxProgress avoids rebuilding and serializing the full queue for
// a position-only heartbeat. Clients apply it only when revision and current
// item still match their authoritative snapshot.
func (h *Handler) publishMusicBoxProgress(roomID string, progress musicbox.ProgressSnapshot) {
	if h == nil || h.Bus == nil || roomID == "" || progress.CurrentItemID == "" {
		return
	}
	if h.MusicBox != nil {
		h.MusicBox.RecordCompactProgressEvent()
	}
	h.Bus.PublishRoom(roomID, eventbus.Event{
		Type:   "music_box_progress",
		RoomID: roomID,
		Data: gin.H{
			"revision":        progress.Revision,
			"current_item_id": progress.CurrentItemID,
			"position_ms":     progress.PositionMS,
		},
	})
}

// musicBoxStatePayload builds the SSE/HTTP snapshot for a room's music box.
func (h *Handler) musicBoxStatePayload(roomID, actorID string) gin.H {
	if h.MusicBox == nil {
		return gin.H{"enabled": false}
	}
	st, items, err := h.MusicBox.State(roomID)
	if err != nil {
		return gin.H{"enabled": h.MusicBox.Enabled()}
	}
	temporaryItems, err := h.MusicBox.TemporaryQueue(roomID)
	if err != nil {
		return gin.H{"enabled": h.MusicBox.Enabled()}
	}
	used, capBytes := h.MusicBox.RoomUsage(roomID)
	allItems := make([]*musicbox.QueueItem, 0, len(items)+len(temporaryItems))
	allItems = append(allItems, items...)
	allItems = append(allItems, temporaryItems...)
	requesters := h.musicBoxRequesterPayloads(
		roomID,
		allItems,
		st.ActivePlaylistOwnerID,
	)
	capabilities := h.musicBoxCapabilities(roomID, actorID, st)
	queuePayload := func(source []*musicbox.QueueItem, active bool) []gin.H {
		queue := make([]gin.H, 0, len(source))
		for _, it := range source {
			payload := gin.H{
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
				"can_remove":       h.musicBoxCanRemoveItem(actorID, it, capabilities),
				"can_play_now":     active && capabilities.CanPlayNow && it.Status == musicbox.StatusReady,
			}
			if requester := requesters[it.AddedByUserID]; requester != nil {
				payload["requested_by"] = requester
			}
			queue = append(queue, payload)
		}
		return queue
	}
	queue := queuePayload(items, true)
	temporaryQueue := queuePayload(
		temporaryItems,
		st.ActiveSourceType == musicbox.ActiveSourceTemporary,
	)
	activeSource := gin.H{
		"type": st.ActiveSourceType,
		"name": st.ActivePlaylistName,
	}
	if st.ActiveSourceType == musicbox.ActiveSourceTemporary {
		activeSource["name"] = musicBoxRequestQueueDisplayName
	} else {
		activeSource["playlist_id"] = st.ActivePlaylistID
		activeSource["snapshot_id"] = st.ActiveSnapshotID
		activeSource["owner_user_id"] = st.ActivePlaylistOwnerID
		if st.ActivePlaylistCreatedAt > 0 {
			activeSource["created_at"] = formatMillis(st.ActivePlaylistCreatedAt)
		}
		if owner := requesters[st.ActivePlaylistOwnerID]; owner != nil {
			activeSource["owner"] = owner
		}
	}
	return gin.H{
		"enabled":       h.MusicBox.Enabled(),
		"revision":      st.Revision,
		"active_source": activeSource,
		"temporary_playlist": gin.H{
			"queued_count": len(temporaryQueue),
			"capabilities": gin.H{
				"can_enqueue":  capabilities.CanEnqueue,
				"can_switch":   capabilities.CanSwitch,
				"can_reorder":  capabilities.CanReorder && st.ActiveSourceType == musicbox.ActiveSourceTemporary,
				"can_clear":    capabilities.CanClear,
				"can_play_now": capabilities.CanPlayNow && st.ActiveSourceType == musicbox.ActiveSourceTemporary,
			},
		},
		"playback": gin.H{
			"state":           string(st.State),
			"current_item_id": st.CurrentItemID,
			"position_ms":     st.PositionMS,
			"volume":          st.Volume,
			"mode":            string(st.PlaybackMode),
			"can_previous":    st.CurrentItemID != "" && len(queue) > 0,
			"can_next":        len(queue) > 0,
			"capabilities": gin.H{
				"can_control":     capabilities.CanControl,
				"can_change_mode": capabilities.CanChangeMode,
				"can_reorder":     capabilities.CanReorder,
				"allowed_modes":   capabilities.AllowedModes,
			},
			"updated_at": formatMillis(st.UpdatedAt),
		},
		"queue":           queue,
		"temporary_queue": temporaryQueue,
		"usage":           gin.H{"used_bytes": used, "limit_bytes": capBytes},
	}
}

func (h *Handler) musicBoxRequesterPayloads(
	roomID string,
	items []*musicbox.QueueItem,
	extraUserIDs ...string,
) map[string]gin.H {
	result := make(map[string]gin.H)
	if h == nil || h.DB == nil {
		return result
	}
	ids := make([]string, 0, len(items)+len(extraUserIDs))
	seen := make(map[string]struct{}, len(items)+len(extraUserIDs))
	for _, item := range items {
		if item.AddedByUserID == "" {
			continue
		}
		if _, exists := seen[item.AddedByUserID]; exists {
			continue
		}
		seen[item.AddedByUserID] = struct{}{}
		ids = append(ids, item.AddedByUserID)
	}
	for _, id := range extraUserIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return result
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, roomID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := h.DB.Query(
		`SELECT u.id, u.username, u.display_name, u.avatar_url,
		        u.default_avatar_key, rm.room_display_name
		 FROM users u
		 LEFT JOIN room_memberships rm
		   ON rm.room_id = ? AND rm.user_id = u.id
		 WHERE u.id IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var id, username string
		var displayName, avatarURL, defaultAvatar, roomDisplayName sql.NullString
		if err := rows.Scan(
			&id,
			&username,
			&displayName,
			&avatarURL,
			&defaultAvatar,
			&roomDisplayName,
		); err != nil {
			continue
		}
		globalName := username
		if displayName.Valid && strings.TrimSpace(displayName.String) != "" {
			globalName = displayName.String
		}
		name := globalName
		if roomDisplayName.Valid && strings.TrimSpace(roomDisplayName.String) != "" {
			name = roomDisplayName.String
		}
		payload := gin.H{
			"user_id":            id,
			"username":           username,
			"display_name":       name,
			"avatar_label":       globalName,
			"default_avatar_key": defaultAvatar.String,
		}
		if avatarURL.Valid && avatarURL.String != "" {
			payload["avatar_url"] = avatarURL.String
		}
		result[id] = payload
	}
	return result
}
