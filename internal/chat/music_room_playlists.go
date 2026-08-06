package chat

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/zhuangkaiyi/gang-chat/server/internal/musicbox"
)

func (h *Handler) listRoomMusicPlaylists(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	page, pageSize := musicPlaylistPage(c)
	result, err := h.Playlists.ListRoomPlaylists(
		c.Request.Context(),
		roomID,
		page,
		pageSize,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "list room music playlists failed")
		return
	}
	items := make([]gin.H, 0, len(result.Items))
	for _, playlist := range result.Items {
		items = append(items, musicPlaylistPayload(playlist))
	}
	c.JSON(http.StatusOK, gin.H{
		"playlists": items,
		"pagination": musicPlaylistPaginationPayload(
			result.Page,
			result.PageSize,
			result.Total,
			result.HasMore,
		),
		"limits": gin.H{
			"max_playlists":      musicbox.MaxRoomPlaylists,
			"max_playlist_items": musicbox.MaxPlaylistItems,
		},
	})
}

func (h *Handler) createRoomMusicPlaylist(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomPlaylistAdmin(c, roomID) {
		return
	}
	var req createMusicPlaylistRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	name := strings.TrimSpace(req.Name)
	req.ImportPlaylistID = strings.TrimSpace(req.ImportPlaylistID)
	if name == "" || utf8.RuneCountInString(name) > maxPlaylistNameRunes {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "playlist name must contain 1 to 64 characters")
		return
	}
	var playlist musicbox.PlaylistSummary
	var err error
	if req.ImportPlaylistID == "" {
		playlist, err = h.Playlists.CreateRoomPlaylist(
			c.Request.Context(),
			roomID,
			name,
		)
	} else {
		playlist, err = h.Playlists.CreateRoomPlaylistFromUserPlaylist(
			c.Request.Context(),
			roomID,
			name,
			currentUserID(c),
			req.ImportPlaylistID,
		)
	}
	if err != nil {
		h.writeRoomPlaylistMutationError(c, err, "create room music playlist failed")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"playlist": musicPlaylistPayload(playlist)})
}

func (h *Handler) mergeRoomMusicPlaylists(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomPlaylistAdmin(c, roomID) {
		return
	}
	var req mergeMusicPlaylistsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	name := strings.TrimSpace(req.Name)
	playlistIDs := uniqueMusicPlaylistStrings(req.PlaylistIDs)
	if name == "" || utf8.RuneCountInString(name) > maxPlaylistNameRunes {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "playlist name must contain 1 to 64 characters")
		return
	}
	if len(playlistIDs) < 2 ||
		len(playlistIDs) != len(req.PlaylistIDs) ||
		len(playlistIDs) > musicbox.MaxRoomPlaylists {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "playlist_ids must contain 2 to 50 unique values")
		return
	}
	result, err := h.Playlists.MergeRoomPlaylists(
		c.Request.Context(),
		roomID,
		currentUserID(c),
		name,
		playlistIDs,
	)
	if err != nil {
		h.writeMusicPlaylistMergeError(c, err, "merge room music playlists failed")
		return
	}
	c.JSON(http.StatusCreated, musicPlaylistMergePayload(result))
}

func (h *Handler) cloneRoomMusicPlaylistToMe(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	playlist, err := h.Playlists.CloneRoomPlaylistToUser(
		c.Request.Context(),
		currentUserID(c),
		roomID,
		c.Param("playlist_id"),
	)
	if err != nil {
		switch {
		case errors.Is(err, musicbox.ErrPlaylistNotFound):
			h.jsonError(c, http.StatusNotFound, "not_found", "music playlist not found")
		case errors.Is(err, musicbox.ErrPlaylistLimit):
			h.jsonError(c, http.StatusConflict, "playlist_limit_reached", "personal playlist limit reached")
		case errors.Is(err, musicbox.ErrPlaylistItemLimit):
			h.jsonError(c, http.StatusConflict, "playlist_item_limit_reached", "playlist item limit reached")
		case errors.Is(err, musicbox.ErrPlaylistName):
			h.jsonError(c, http.StatusConflict, "playlist_name_conflict", "playlist name conflict")
		default:
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "clone room music playlist failed")
		}
		return
	}
	c.JSON(http.StatusCreated, gin.H{"playlist": musicPlaylistPayload(playlist)})
}

func (h *Handler) deleteRoomMusicPlaylist(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomPlaylistAdmin(c, roomID) {
		return
	}
	deleted, err := h.Playlists.DeleteRoomPlaylist(
		c.Request.Context(),
		roomID,
		c.Param("playlist_id"),
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "delete room music playlist failed")
		return
	}
	if !deleted {
		h.jsonError(c, http.StatusNotFound, "not_found", "music playlist not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) renameRoomMusicPlaylist(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomPlaylistAdmin(c, roomID) {
		return
	}
	var req renameMusicPlaylistRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || utf8.RuneCountInString(name) > maxPlaylistNameRunes {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "playlist name must contain 1 to 64 characters")
		return
	}
	playlist, err := h.Playlists.RenameRoomPlaylist(
		c.Request.Context(),
		roomID,
		c.Param("playlist_id"),
		name,
	)
	if err != nil {
		h.writeRoomPlaylistMutationError(c, err, "rename room music playlist failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"playlist": musicPlaylistPayload(playlist)})
}

func (h *Handler) reorderRoomMusicPlaylists(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomPlaylistAdmin(c, roomID) {
		return
	}
	var req reorderMusicPlaylistsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	var err error
	if strings.TrimSpace(req.PlaylistID) != "" || strings.TrimSpace(req.Direction) != "" {
		direction := roomMusicPlaylistDirection(c, req.Direction)
		if direction == 0 {
			return
		}
		err = h.Playlists.MoveRoomPlaylist(
			c.Request.Context(),
			roomID,
			strings.TrimSpace(req.PlaylistID),
			direction,
		)
	} else {
		playlistIDs := uniqueMusicPlaylistStrings(req.PlaylistIDs)
		if len(playlistIDs) == 0 ||
			len(playlistIDs) != len(req.PlaylistIDs) ||
			len(playlistIDs) > musicbox.MaxRoomPlaylists {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "playlist_ids must contain 1 to 50 unique values")
			return
		}
		err = h.Playlists.PinRoomPlaylists(c.Request.Context(), roomID, playlistIDs)
	}
	if err != nil {
		h.writeRoomPlaylistOrderError(c, err, "reorder room music playlists failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) getRoomMusicPlaylist(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	page, pageSize := musicPlaylistPage(c)
	keyword := strings.TrimSpace(c.Query("keyword"))
	if utf8.RuneCountInString(keyword) > maxPlaylistKeywordRunes {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "filter keyword is too long")
		return
	}
	source := strings.TrimSpace(c.Query("source"))
	if source != "" && !allowedMusicPlaylistSource(source) {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid music source")
		return
	}
	result, err := h.Playlists.RoomPlaylistItems(
		c.Request.Context(),
		roomID,
		c.Param("playlist_id"),
		keyword,
		source,
		page,
		pageSize,
	)
	if err != nil {
		if errors.Is(err, musicbox.ErrPlaylistNotFound) {
			h.jsonError(c, http.StatusNotFound, "not_found", "music playlist not found")
		} else {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "load room music playlist failed")
		}
		return
	}
	items := make([]gin.H, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, musicPlaylistItemPayload(item))
	}
	c.JSON(http.StatusOK, gin.H{
		"playlist": musicPlaylistPayload(result.Playlist),
		"items":    items,
		"pagination": musicPlaylistPaginationPayload(
			result.Page,
			result.PageSize,
			result.Total,
			result.HasMore,
		),
	})
}

func (h *Handler) addRoomMusicPlaylistItem(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomPlaylistAdmin(c, roomID) {
		return
	}
	var req addMusicPlaylistItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	trackID := strings.TrimSpace(req.TrackID)
	source := strings.TrimSpace(req.Source)
	title := strings.TrimSpace(req.Title)
	artists := normalizeMusicPlaylistArtists(req.Artists, req.Artist)
	if trackID == "" ||
		utf8.RuneCountInString(trackID) > maxPlaylistTrackIDRunes ||
		!allowedMusicPlaylistSource(source) ||
		title == "" ||
		utf8.RuneCountInString(title) > maxPlaylistTrackTitleRunes ||
		len(artists) > maxPlaylistArtists {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid playlist track")
		return
	}
	for _, artist := range artists {
		if utf8.RuneCountInString(artist) > maxPlaylistArtistRunes {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "artist name is too long")
			return
		}
	}
	if req.DurationMS < 0 {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "duration must not be negative")
		return
	}
	item, err := h.Playlists.AddRoomPlaylistItem(
		c.Request.Context(),
		musicbox.AddPlaylistItemParams{
			RoomID:          roomID,
			AddedByUserID:   currentUserID(c),
			PlaylistID:      c.Param("playlist_id"),
			Source:          source,
			ExternalTrackID: trackID,
			Title:           title,
			Artists:         artists,
			DurationMS:      req.DurationMS,
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, musicbox.ErrPlaylistNotFound):
			h.jsonError(c, http.StatusNotFound, "not_found", "music playlist not found")
		case errors.Is(err, musicbox.ErrPlaylistItemLimit):
			h.jsonError(c, http.StatusConflict, "playlist_item_limit_reached", "playlist item limit reached")
		default:
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "add room music playlist item failed")
		}
		return
	}
	c.JSON(http.StatusCreated, gin.H{"item": musicPlaylistItemPayload(item)})
}

func (h *Handler) deleteRoomMusicPlaylistItem(c *gin.Context) {
	h.deleteRoomMusicPlaylistItems(c, []string{c.Param("item_id")})
}

func (h *Handler) deleteRoomMusicPlaylistItemsBatch(c *gin.Context) {
	var req deleteMusicPlaylistItemsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	h.deleteRoomMusicPlaylistItems(c, req.ItemIDs)
}

func (h *Handler) deleteRoomMusicPlaylistItems(c *gin.Context, itemIDs []string) {
	roomID := c.Param("room_id")
	if !h.requireRoomPlaylistAdmin(c, roomID) {
		return
	}
	itemIDs = uniqueMusicPlaylistStrings(itemIDs)
	if len(itemIDs) == 0 || len(itemIDs) > musicbox.MaxPlaylistItems {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "item_ids must contain 1 to 500 values")
		return
	}
	deleted, err := h.Playlists.DeleteRoomPlaylistItems(
		c.Request.Context(),
		roomID,
		c.Param("playlist_id"),
		itemIDs,
	)
	if err != nil {
		if errors.Is(err, musicbox.ErrPlaylistNotFound) {
			h.jsonError(c, http.StatusNotFound, "not_found", "music playlist not found")
		} else {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "delete room music playlist items failed")
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "deleted": deleted})
}

func (h *Handler) reorderRoomMusicPlaylistItems(c *gin.Context) {
	roomID := c.Param("room_id")
	if !h.requireRoomPlaylistAdmin(c, roomID) {
		return
	}
	var req reorderMusicPlaylistItemsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	var err error
	if strings.TrimSpace(req.ItemID) != "" || strings.TrimSpace(req.Direction) != "" {
		direction := roomMusicPlaylistDirection(c, req.Direction)
		if direction == 0 {
			return
		}
		err = h.Playlists.MoveRoomPlaylistItem(
			c.Request.Context(),
			roomID,
			c.Param("playlist_id"),
			strings.TrimSpace(req.ItemID),
			direction,
		)
	} else {
		itemIDs := uniqueMusicPlaylistStrings(req.ItemIDs)
		if len(itemIDs) != len(req.ItemIDs) || len(itemIDs) > musicbox.MaxPlaylistItems {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "item_ids must be unique")
			return
		}
		err = h.Playlists.ReorderRoomPlaylistItems(
			c.Request.Context(),
			roomID,
			c.Param("playlist_id"),
			itemIDs,
		)
	}
	if err != nil {
		h.writeRoomPlaylistOrderError(c, err, "reorder room music playlist failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) requireRoomPlaylistAdmin(c *gin.Context, roomID string) bool {
	if h.isAdmin(roomID, currentUserID(c)) {
		return true
	}
	h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
	return false
}

func (h *Handler) writeRoomPlaylistMutationError(
	c *gin.Context,
	err error,
	message string,
) {
	switch {
	case errors.Is(err, musicbox.ErrPlaylistNotFound):
		h.jsonError(c, http.StatusNotFound, "not_found", "music playlist not found")
	case errors.Is(err, musicbox.ErrPlaylistLimit):
		h.jsonError(c, http.StatusConflict, "playlist_limit_reached", "playlist limit reached")
	case errors.Is(err, musicbox.ErrPlaylistItemLimit):
		h.jsonError(c, http.StatusConflict, "playlist_item_limit_reached", "playlist item limit reached")
	case errors.Is(err, musicbox.ErrPlaylistName):
		h.jsonError(c, http.StatusConflict, "playlist_name_conflict", "playlist name already exists")
	default:
		h.jsonError(c, http.StatusInternalServerError, "internal_error", message)
	}
}

func (h *Handler) writeRoomPlaylistOrderError(
	c *gin.Context,
	err error,
	message string,
) {
	switch {
	case errors.Is(err, musicbox.ErrPlaylistNotFound):
		h.jsonError(c, http.StatusNotFound, "not_found", "music playlist or item not found")
	case errors.Is(err, musicbox.ErrPlaylistOrder):
		h.jsonError(c, http.StatusConflict, "playlist_order_conflict", "playlist order changed; refresh and try again")
	default:
		h.jsonError(c, http.StatusInternalServerError, "internal_error", message)
	}
}

func roomMusicPlaylistDirection(c *gin.Context, raw string) int {
	switch strings.TrimSpace(raw) {
	case "up":
		return -1
	case "down":
		return 1
	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"code":    "validation_failed",
				"message": "direction must be up or down",
			},
		})
		return 0
	}
}
