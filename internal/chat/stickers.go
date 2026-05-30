package chat

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (h *Handler) listStickerPacks(c *gin.Context) {
	scope := c.Query("scope")
	if scope == "" {
		scope = "personal"
	}
	roomID := c.Query("room_id")
	if scope == "room" && !h.requireRoomAccess(c, roomID) {
		return
	}
	rows, err := h.DB.Query(
		`SELECT id, scope, room_id, name, sort_order, updated_at FROM sticker_packs
		 WHERE (scope = 'personal' AND owner_user_id = ?) OR (scope = 'room' AND room_id = ?)
		 ORDER BY sort_order ASC, updated_at DESC`,
		currentUserID(c), roomID,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "list sticker packs failed")
		return
	}
	defer rows.Close()
	packs := make([]gin.H, 0)
	for rows.Next() {
		var id, packScope, name string
		var packRoomID sql.NullString
		var sortOrder int
		var updatedAt int64
		if err := rows.Scan(&id, &packScope, &packRoomID, &name, &sortOrder, &updatedAt); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "read sticker pack failed")
			return
		}
		packs = append(packs, h.stickerPackPayload(id, packScope, nullableString(packRoomID), name, sortOrder, updatedAt))
	}
	c.JSON(http.StatusOK, gin.H{"packs": packs})
}

func (h *Handler) createStickerPack(c *gin.Context) {
	var req stickerPackRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "sticker pack name is required")
		return
	}
	if req.Scope == "" {
		req.Scope = "personal"
	}
	if req.Scope == "room" {
		if !h.isAdmin(req.RoomID, currentUserID(c)) {
			h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
			return
		}
	} else if req.Scope != "personal" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid sticker pack scope")
		return
	}
	id := newID("stkp")
	now := nowMillis()
	sortOrder := 10
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	var ownerID any = currentUserID(c)
	var roomID any
	if req.Scope == "room" {
		ownerID = nil
		roomID = req.RoomID
	}
	_, err := h.DB.Exec(
		`INSERT INTO sticker_packs (id, owner_user_id, room_id, scope, name, sort_order, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, ownerID, roomID, req.Scope, strings.TrimSpace(req.Name), sortOrder, now, now,
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "create sticker pack failed")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"pack": h.stickerPackByID(id)})
}

func (h *Handler) updateStickerPack(c *gin.Context) {
	if !h.canManageStickerPack(c.Param("pack_id"), currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot manage sticker pack")
		return
	}
	var req stickerPackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	sortOrder := 10
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	_, err := h.DB.Exec(
		`UPDATE sticker_packs SET name = COALESCE(NULLIF(?, ''), name), sort_order = ?, updated_at = ? WHERE id = ?`,
		strings.TrimSpace(req.Name), sortOrder, nowMillis(), c.Param("pack_id"),
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "update sticker pack failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"pack": h.stickerPackByID(c.Param("pack_id"))})
}

func (h *Handler) deleteStickerPack(c *gin.Context) {
	if !h.canManageStickerPack(c.Param("pack_id"), currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot manage sticker pack")
		return
	}
	_, _ = h.DB.Exec(`DELETE FROM sticker_packs WHERE id = ?`, c.Param("pack_id"))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) addSticker(c *gin.Context) {
	if !h.canManageStickerPack(c.Param("pack_id"), currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot manage sticker pack")
		return
	}
	var req stickerRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.AssetID == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "asset_id is required")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "sticker"
	}
	sortOrder := 10
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	id := newID("stk")
	_, err := h.DB.Exec(
		`INSERT INTO stickers (id, pack_id, asset_id, name, sort_order, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, c.Param("pack_id"), req.AssetID, name, sortOrder, nowMillis(),
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "add sticker failed")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"sticker": gin.H{"id": id, "asset_id": req.AssetID, "name": name, "sort_order": sortOrder}})
}

func (h *Handler) deleteSticker(c *gin.Context) {
	if !h.canManageStickerPack(c.Param("pack_id"), currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot manage sticker pack")
		return
	}
	_, _ = h.DB.Exec(`DELETE FROM stickers WHERE id = ? AND pack_id = ?`, c.Param("sticker_id"), c.Param("pack_id"))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) saveSticker(c *gin.Context) {
	roomID := c.Param("room_id")
	userID := currentUserID(c)
	if !h.requireRoomAccess(c, roomID) {
		return
	}
	var req saveStickerRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.StickerID) == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "sticker_id is required")
		return
	}

	assetID, sourceName, ok := h.visibleStickerAsset(roomID, userID, strings.TrimSpace(req.StickerID))
	if !ok {
		h.jsonError(c, http.StatusNotFound, "not_found", "sticker not found")
		return
	}

	scope := req.TargetScope
	if scope == "" {
		scope = "personal"
	}
	var packID string
	var err error
	switch scope {
	case "personal":
		if req.TargetPackID != "" {
			if !h.canManageStickerPack(req.TargetPackID, userID) || !h.packHasScope(req.TargetPackID, "personal", "") {
				h.jsonError(c, http.StatusForbidden, "forbidden", "cannot manage sticker pack")
				return
			}
			packID = req.TargetPackID
		} else {
			packID, err = h.ensureDefaultStickerPack("personal", userID, "", "Saved Stickers")
		}
	case "room":
		if !h.isAdmin(roomID, userID) {
			h.jsonError(c, http.StatusForbidden, "forbidden", "admin required")
			return
		}
		if req.TargetPackID != "" {
			if !h.canManageStickerPack(req.TargetPackID, userID) || !h.packHasScope(req.TargetPackID, "room", roomID) {
				h.jsonError(c, http.StatusForbidden, "forbidden", "cannot manage sticker pack")
				return
			}
			packID = req.TargetPackID
		} else {
			packID, err = h.ensureDefaultStickerPack("room", "", roomID, "Room Saved Stickers")
		}
	default:
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid sticker target_scope")
		return
	}
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "prepare sticker pack failed")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = sourceName
	}
	if name == "" {
		name = "sticker"
	}
	sortOrder := 10
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	stickerID := newID("stk")
	_, err = h.DB.Exec(
		`INSERT INTO stickers (id, pack_id, asset_id, name, sort_order, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		stickerID, packID, assetID, name, sortOrder, nowMillis(),
	)
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "save sticker failed")
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"pack":    h.stickerPackByID(packID),
		"sticker": gin.H{"id": stickerID, "asset_id": assetID, "name": name, "sort_order": sortOrder},
	})
}

func (h *Handler) stickerPackByID(id string) gin.H {
	var scope, name string
	var roomID sql.NullString
	var sortOrder int
	var updatedAt int64
	_ = h.DB.QueryRow(`SELECT scope, room_id, name, sort_order, updated_at FROM sticker_packs WHERE id = ?`, id).Scan(&scope, &roomID, &name, &sortOrder, &updatedAt)
	return h.stickerPackPayload(id, scope, nullableString(roomID), name, sortOrder, updatedAt)
}

func (h *Handler) stickerPackPayload(id, scope string, roomID *string, name string, sortOrder int, updatedAt int64) gin.H {
	rows, _ := h.DB.Query(
		`SELECT s.id, s.name, s.sort_order, a.id, a.url, a.thumbnail_url, a.mime_type, a.width, a.height, a.created_at
		 FROM stickers s JOIN assets a ON a.id = s.asset_id
		 WHERE s.pack_id = ? ORDER BY s.sort_order ASC`,
		id,
	)
	stickers := make([]gin.H, 0)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var stickerID, stickerName, assetID, url, thumb, mimeType string
			var stickerSort int
			var width, height sql.NullInt64
			var createdAt int64
			if err := rows.Scan(&stickerID, &stickerName, &stickerSort, &assetID, &url, &thumb, &mimeType, &width, &height, &createdAt); err == nil {
				stickers = append(stickers, gin.H{
					"id": stickerID, "name": stickerName, "sort_order": stickerSort,
					"asset": gin.H{"id": assetID, "url": url, "thumbnail_url": thumb, "mime_type": mimeType, "width": nullableInt64(width), "height": nullableInt64(height), "created_at": formatMillis(createdAt)},
				})
			}
		}
	}
	return gin.H{"id": id, "scope": scope, "room_id": roomID, "name": name, "stickers": stickers, "sort_order": sortOrder, "updated_at": formatMillis(updatedAt)}
}

func (h *Handler) canManageStickerPack(packID, userID string) bool {
	var scope string
	var ownerID, roomID sql.NullString
	if err := h.DB.QueryRow(`SELECT scope, owner_user_id, room_id FROM sticker_packs WHERE id = ?`, packID).Scan(&scope, &ownerID, &roomID); err != nil {
		return false
	}
	if scope == "personal" {
		return ownerID.Valid && ownerID.String == userID
	}
	return roomID.Valid && h.isAdmin(roomID.String, userID)
}

func (h *Handler) visibleStickerAsset(roomID, userID, stickerID string) (string, string, bool) {
	var assetID, name string
	err := h.DB.QueryRow(
		`SELECT s.asset_id, s.name
		 FROM stickers s JOIN sticker_packs p ON p.id = s.pack_id
		 WHERE s.id = ?
		   AND (
		     (p.scope = 'personal' AND p.owner_user_id = ?)
		     OR (p.scope = 'room' AND p.room_id = ?)
		   )`,
		stickerID, userID, roomID,
	).Scan(&assetID, &name)
	return assetID, name, err == nil
}

func (h *Handler) ensureDefaultStickerPack(scope, ownerUserID, roomID, name string) (string, error) {
	var id string
	if scope == "personal" {
		err := h.DB.QueryRow(
			`SELECT id FROM sticker_packs WHERE scope = 'personal' AND owner_user_id = ? AND name = ? ORDER BY created_at ASC LIMIT 1`,
			ownerUserID, name,
		).Scan(&id)
		if err == nil {
			return id, nil
		}
		if err != sql.ErrNoRows {
			return "", err
		}
		now := nowMillis()
		id = newID("stkp")
		_, err = h.DB.Exec(
			`INSERT INTO sticker_packs (id, owner_user_id, scope, name, sort_order, created_at, updated_at)
			 VALUES (?, ?, 'personal', ?, 10, ?, ?)`,
			id, ownerUserID, name, now, now,
		)
		return id, err
	}

	err := h.DB.QueryRow(
		`SELECT id FROM sticker_packs WHERE scope = 'room' AND room_id = ? AND name = ? ORDER BY created_at ASC LIMIT 1`,
		roomID, name,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	now := nowMillis()
	id = newID("stkp")
	_, err = h.DB.Exec(
		`INSERT INTO sticker_packs (id, room_id, scope, name, sort_order, created_at, updated_at)
		 VALUES (?, ?, 'room', ?, 10, ?, ?)`,
		id, roomID, name, now, now,
	)
	return id, err
}

func (h *Handler) packHasScope(packID, scope, roomID string) bool {
	var count int
	if scope == "room" {
		_ = h.DB.QueryRow(`SELECT COUNT(*) FROM sticker_packs WHERE id = ? AND scope = 'room' AND room_id = ?`, packID, roomID).Scan(&count)
		return count > 0
	}
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM sticker_packs WHERE id = ? AND scope = ?`, packID, scope).Scan(&count)
	return count > 0
}
