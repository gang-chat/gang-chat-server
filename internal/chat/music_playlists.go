package chat

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/zhuangkaiyi/gang-chat/server/internal/musicbox"
)

const (
	maxPlaylistNameRunes       = 64
	maxPlaylistKeywordRunes    = 128
	maxPlaylistTrackIDRunes    = 256
	maxPlaylistTrackTitleRunes = 512
	maxPlaylistArtistRunes     = 128
	maxPlaylistArtists         = 16
)

type createMusicPlaylistRequest struct {
	Name string `json:"name"`
}

type renameMusicPlaylistRequest struct {
	Name string `json:"name"`
}

type addMusicPlaylistItemRequest struct {
	TrackID    string   `json:"track_id"`
	Source     string   `json:"source"`
	Title      string   `json:"title"`
	Artists    []string `json:"artists"`
	Artist     string   `json:"artist"`
	DurationMS int64    `json:"duration_ms"`
}

type deleteMusicPlaylistItemsRequest struct {
	ItemIDs []string `json:"item_ids"`
}

type reorderMusicPlaylistsRequest struct {
	PlaylistIDs []string `json:"playlist_ids"`
	PlaylistID  string   `json:"playlist_id"`
	Direction   string   `json:"direction"`
}

type reorderMusicPlaylistItemsRequest struct {
	ItemIDs   []string `json:"item_ids"`
	ItemID    string   `json:"item_id"`
	Direction string   `json:"direction"`
}

func (h *Handler) listMyMusicPlaylists(c *gin.Context) {
	page, pageSize := musicPlaylistPage(c)
	result, err := h.Playlists.ListUserPlaylists(
		c.Request.Context(),
		currentUserID(c),
		page,
		pageSize,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "list music playlists failed")
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
			"max_playlists":      musicbox.MaxUserPlaylists,
			"max_playlist_items": musicbox.MaxPlaylistItems,
		},
	})
}

func (h *Handler) createMyMusicPlaylist(c *gin.Context) {
	var req createMusicPlaylistRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || utf8.RuneCountInString(name) > maxPlaylistNameRunes {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "playlist name must contain 1 to 64 characters")
		return
	}
	playlist, err := h.Playlists.CreateUserPlaylist(
		c.Request.Context(),
		currentUserID(c),
		name,
	)
	if err != nil {
		switch {
		case errors.Is(err, musicbox.ErrPlaylistLimit):
			h.jsonError(c, http.StatusConflict, "playlist_limit_reached", "playlist limit reached")
		case isDuplicateMusicPlaylistName(err):
			h.jsonError(c, http.StatusConflict, "playlist_name_conflict", "playlist name already exists")
		default:
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "create music playlist failed")
		}
		return
	}
	c.JSON(http.StatusCreated, gin.H{"playlist": musicPlaylistPayload(playlist)})
}

func (h *Handler) deleteMyMusicPlaylist(c *gin.Context) {
	deleted, err := h.Playlists.DeleteUserPlaylist(
		c.Request.Context(),
		currentUserID(c),
		c.Param("playlist_id"),
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "delete music playlist failed")
		return
	}
	if !deleted {
		h.jsonError(c, http.StatusNotFound, "not_found", "music playlist not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) renameMyMusicPlaylist(c *gin.Context) {
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
	playlist, err := h.Playlists.RenameUserPlaylist(
		c.Request.Context(),
		currentUserID(c),
		c.Param("playlist_id"),
		name,
	)
	if err != nil {
		switch {
		case errors.Is(err, musicbox.ErrPlaylistNotFound):
			h.jsonError(c, http.StatusNotFound, "not_found", "music playlist not found")
		case isDuplicateMusicPlaylistName(err):
			h.jsonError(c, http.StatusConflict, "playlist_name_conflict", "playlist name already exists")
		default:
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "rename music playlist failed")
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"playlist": musicPlaylistPayload(playlist)})
}

func (h *Handler) reorderMyMusicPlaylists(c *gin.Context) {
	var req reorderMusicPlaylistsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	var err error
	if strings.TrimSpace(req.PlaylistID) != "" ||
		strings.TrimSpace(req.Direction) != "" {
		direction := 0
		switch strings.TrimSpace(req.Direction) {
		case "up":
			direction = -1
		case "down":
			direction = 1
		default:
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "direction must be up or down")
			return
		}
		err = h.Playlists.MoveUserPlaylist(
			c.Request.Context(),
			currentUserID(c),
			strings.TrimSpace(req.PlaylistID),
			direction,
		)
	} else {
		playlistIDs := uniqueMusicPlaylistStrings(req.PlaylistIDs)
		if len(playlistIDs) == 0 ||
			len(playlistIDs) != len(req.PlaylistIDs) ||
			len(playlistIDs) > musicbox.MaxUserPlaylists {
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "playlist_ids must contain 1 to 50 unique values")
			return
		}
		err = h.Playlists.PinUserPlaylists(
			c.Request.Context(),
			currentUserID(c),
			playlistIDs,
		)
	}
	if err != nil {
		switch {
		case errors.Is(err, musicbox.ErrPlaylistNotFound):
			h.jsonError(c, http.StatusNotFound, "not_found", "music playlist not found")
		case errors.Is(err, musicbox.ErrPlaylistOrder):
			h.jsonError(c, http.StatusConflict, "playlist_order_conflict", "playlist order changed; refresh and try again")
		default:
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "reorder music playlists failed")
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) getMyMusicPlaylist(c *gin.Context) {
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
	result, err := h.Playlists.UserPlaylistItems(
		c.Request.Context(),
		currentUserID(c),
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
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "load music playlist failed")
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

func (h *Handler) searchMyMusicPlaylistTracks(c *gin.Context) {
	if h.MusicBox == nil {
		h.jsonError(c, http.StatusServiceUnavailable, "music_box_unavailable", "music search is not available")
		return
	}
	keyword := strings.TrimSpace(c.Query("keyword"))
	if keyword == "" || utf8.RuneCountInString(keyword) > maxPlaylistKeywordRunes {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "keyword is required and must not exceed 128 characters")
		return
	}
	source := strings.TrimSpace(c.Query("source"))
	if source != "" && !allowedMusicPlaylistSource(source) {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid music source")
		return
	}
	count, _ := strconv.Atoi(c.Query("count"))
	page, _ := strconv.Atoi(c.Query("page"))
	if count < 1 || count > musicbox.MaxPlaylistPage {
		count = 20
	}
	if page < 1 {
		page = 1
	}
	results, err := h.MusicBox.Search(c.Request.Context(), source, keyword, count, page)
	if err != nil {
		h.jsonError(c, http.StatusBadGateway, "upstream_error", "music search failed: "+err.Error())
		return
	}
	tracks := make([]gin.H, 0, len(results))
	for _, result := range results {
		tracks = append(tracks, gin.H{
			"track_id": result.TrackID,
			"name":     result.Name,
			"artists":  result.Artists,
			"source":   result.Source,
		})
	}
	c.JSON(http.StatusOK, gin.H{"results": tracks})
}

func (h *Handler) addMyMusicPlaylistItem(c *gin.Context) {
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
	item, err := h.Playlists.AddUserPlaylistItem(
		c.Request.Context(),
		musicbox.AddPlaylistItemParams{
			OwnerUserID:     currentUserID(c),
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
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "add music playlist item failed")
		}
		return
	}
	c.JSON(http.StatusCreated, gin.H{"item": musicPlaylistItemPayload(item)})
}

func (h *Handler) deleteMyMusicPlaylistItem(c *gin.Context) {
	h.deleteMyMusicPlaylistItems(c, []string{c.Param("item_id")})
}

func (h *Handler) deleteMyMusicPlaylistItemsBatch(c *gin.Context) {
	var req deleteMusicPlaylistItemsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	h.deleteMyMusicPlaylistItems(c, req.ItemIDs)
}

func (h *Handler) deleteMyMusicPlaylistItems(c *gin.Context, itemIDs []string) {
	itemIDs = uniqueMusicPlaylistStrings(itemIDs)
	if len(itemIDs) == 0 || len(itemIDs) > musicbox.MaxPlaylistItems {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "item_ids must contain 1 to 500 values")
		return
	}
	deleted, err := h.Playlists.DeleteUserPlaylistItems(
		c.Request.Context(),
		currentUserID(c),
		c.Param("playlist_id"),
		itemIDs,
	)
	if err != nil {
		if errors.Is(err, musicbox.ErrPlaylistNotFound) {
			h.jsonError(c, http.StatusNotFound, "not_found", "music playlist not found")
		} else {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "delete music playlist items failed")
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "deleted": deleted})
}

func (h *Handler) reorderMyMusicPlaylistItems(c *gin.Context) {
	var req reorderMusicPlaylistItemsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	var err error
	if strings.TrimSpace(req.ItemID) != "" || strings.TrimSpace(req.Direction) != "" {
		direction := 0
		switch strings.TrimSpace(req.Direction) {
		case "up":
			direction = -1
		case "down":
			direction = 1
		default:
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "direction must be up or down")
			return
		}
		err = h.Playlists.MoveUserPlaylistItem(
			c.Request.Context(),
			currentUserID(c),
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
		err = h.Playlists.ReorderUserPlaylistItems(
			c.Request.Context(),
			currentUserID(c),
			c.Param("playlist_id"),
			itemIDs,
		)
	}
	if err != nil {
		switch {
		case errors.Is(err, musicbox.ErrPlaylistNotFound):
			h.jsonError(c, http.StatusNotFound, "not_found", "music playlist or item not found")
		case errors.Is(err, musicbox.ErrPlaylistOrder):
			h.jsonError(c, http.StatusConflict, "playlist_order_conflict", "playlist order changed; refresh and try again")
		default:
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "reorder music playlist failed")
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func musicPlaylistPage(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = musicbox.DefaultPlaylistPage
	}
	if pageSize > musicbox.MaxPlaylistPage {
		pageSize = musicbox.MaxPlaylistPage
	}
	return page, pageSize
}

func musicPlaylistPayload(playlist musicbox.PlaylistSummary) gin.H {
	return gin.H{
		"id":          playlist.ID,
		"name":        playlist.Name,
		"description": playlist.Description,
		"revision":    playlist.Revision,
		"item_count":  playlist.ItemCount,
		"created_at":  formatMillis(playlist.CreatedAt),
		"updated_at":  formatMillis(playlist.UpdatedAt),
	}
}

func musicPlaylistItemPayload(item musicbox.PlaylistItem) gin.H {
	return gin.H{
		"id":          item.ID,
		"playlist_id": item.PlaylistID,
		"track_id":    item.ExternalTrackID,
		"source":      item.Source,
		"title":       item.Title,
		"artists":     item.Artists,
		"duration_ms": item.DurationMS,
		"sort_order":  item.SortOrder,
		"created_at":  formatMillis(item.CreatedAt),
	}
}

func musicPlaylistPaginationPayload(page, pageSize, total int, hasMore bool) gin.H {
	return gin.H{
		"page":      page,
		"page_size": pageSize,
		"total":     total,
		"has_more":  hasMore,
	}
}

func allowedMusicPlaylistSource(source string) bool {
	return allowed(source, "netease", "bilibili", "tencent")
}

func normalizeMusicPlaylistArtists(values []string, fallback string) []string {
	if len(values) == 0 && strings.TrimSpace(fallback) != "" {
		values = strings.Split(fallback, "、")
	}
	return uniqueMusicPlaylistStrings(values)
}

func uniqueMusicPlaylistStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func isDuplicateMusicPlaylistName(err error) bool {
	var mysqlErr *mysqlDriver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}
