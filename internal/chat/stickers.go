package chat

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

func (h *Handler) listStickerPacks(c *gin.Context) {
	scope := c.Query("scope")
	if scope == "" {
		scope = "personal"
	}
	roomID := c.Query("room_id")
	var rows *sql.Rows
	var err error
	switch scope {
	case "personal":
		rows, err = h.DB.Query(
			`SELECT id, scope, room_id, name, sort_order, updated_at FROM sticker_packs
			 WHERE scope = 'personal' AND owner_user_id = ?
			 ORDER BY sort_order ASC, updated_at DESC`,
			currentUserID(c),
		)
	case "room":
		if !h.requireRoomAccess(c, roomID) {
			return
		}
		rows, err = h.DB.Query(
			`SELECT id, scope, room_id, name, sort_order, updated_at FROM sticker_packs
			 WHERE scope = 'room' AND room_id = ?
			 ORDER BY sort_order ASC, updated_at DESC`,
			roomID,
		)
	default:
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "invalid sticker pack scope")
		return
	}
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "list sticker packs failed")
		return
	}
	defer rows.Close()
	packs := make([]stickerPackData, 0)
	for rows.Next() {
		var id, packScope, name string
		var packRoomID sql.NullString
		var sortOrder int
		var updatedAt int64
		if err := rows.Scan(&id, &packScope, &packRoomID, &name, &sortOrder, &updatedAt); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "read sticker pack failed")
			return
		}
		packs = append(packs, stickerPackData{
			ID:        id,
			Scope:     packScope,
			RoomID:    nullableString(packRoomID),
			Name:      name,
			SortOrder: sortOrder,
			UpdatedAt: updatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"packs": h.stickerPackPayloads(packs)})
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
	rawBody, ok := h.bindJSON(c, &req)
	if !ok {
		return
	}
	if h.replayIdempotency(c, rawBody) {
		return
	}
	if req.AssetID == "" {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "asset_id is required")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "sticker"
	}
	name = h.uniqueStickerName(c.Param("pack_id"), name, "")
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
	h.touchStickerPack(c.Param("pack_id"))
	h.idempotentJSON(c, http.StatusCreated, rawBody, gin.H{"sticker": h.stickerPayload(id)})
}

func (h *Handler) updateSticker(c *gin.Context) {
	packID := c.Param("pack_id")
	stickerID := c.Param("sticker_id")
	if !h.canManageStickerPack(packID, currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot manage sticker pack")
		return
	}
	var req stickerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.jsonError(c, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	var exists int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM stickers WHERE id = ? AND pack_id = ?`, stickerID, packID).Scan(&exists); err != nil || exists == 0 {
		h.jsonError(c, http.StatusNotFound, "not_found", "sticker not found")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name != "" {
		name = h.uniqueStickerName(packID, name, stickerID)
		if _, err := h.DB.Exec(`UPDATE stickers SET name = ? WHERE id = ? AND pack_id = ?`, name, stickerID, packID); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "update sticker failed")
			return
		}
	}
	if req.SortOrder != nil {
		if _, err := h.DB.Exec(`UPDATE stickers SET sort_order = ? WHERE id = ? AND pack_id = ?`, *req.SortOrder, stickerID, packID); err != nil {
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "update sticker failed")
			return
		}
	}
	h.touchStickerPack(packID)
	c.JSON(http.StatusOK, gin.H{"sticker": h.stickerPayload(stickerID)})
}

func (h *Handler) reorderStickers(c *gin.Context) {
	packID := c.Param("pack_id")
	if !h.canManageStickerPack(packID, currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot manage sticker pack")
		return
	}
	var req stickerReorderRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.StickerIDs) == 0 {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "sticker_ids are required")
		return
	}
	seen := make(map[string]bool, len(req.StickerIDs))
	tx, err := h.DB.Begin()
	if err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "reorder stickers failed")
		return
	}
	for index, rawID := range req.StickerIDs {
		stickerID := strings.TrimSpace(rawID)
		if stickerID == "" || seen[stickerID] {
			_ = tx.Rollback()
			h.jsonError(c, http.StatusBadRequest, "validation_failed", "sticker_ids must be unique")
			return
		}
		seen[stickerID] = true
		result, err := tx.Exec(`UPDATE stickers SET sort_order = ? WHERE id = ? AND pack_id = ?`, (index+1)*10, stickerID, packID)
		if err != nil {
			_ = tx.Rollback()
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "reorder stickers failed")
			return
		}
		changed, _ := result.RowsAffected()
		if changed == 0 {
			_ = tx.Rollback()
			h.jsonError(c, http.StatusNotFound, "not_found", "sticker not found")
			return
		}
	}
	if _, err := tx.Exec(`UPDATE sticker_packs SET updated_at = ? WHERE id = ?`, nowMillis(), packID); err != nil {
		_ = tx.Rollback()
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "reorder stickers failed")
		return
	}
	if err := tx.Commit(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "reorder stickers failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"pack": h.stickerPackByID(packID)})
}

func (h *Handler) deleteSticker(c *gin.Context) {
	if !h.canManageStickerPack(c.Param("pack_id"), currentUserID(c)) {
		h.jsonError(c, http.StatusForbidden, "forbidden", "cannot manage sticker pack")
		return
	}
	if _, err := h.DB.Exec(`DELETE FROM stickers WHERE id = ? AND pack_id = ?`, c.Param("sticker_id"), c.Param("pack_id")); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "delete sticker failed")
		return
	}
	h.touchStickerPack(c.Param("pack_id"))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) downloadStickers(c *gin.Context) {
	ids := parseStickerIDList(c.Query("ids"))
	if len(ids) == 0 {
		h.jsonError(c, http.StatusBadRequest, "validation_failed", "ids are required")
		return
	}
	items := make([]downloadableSticker, 0, len(ids))
	for _, id := range ids {
		item, ok := h.downloadableSticker(id, currentUserID(c))
		if !ok {
			h.jsonError(c, http.StatusNotFound, "not_found", "sticker not found")
			return
		}
		items = append(items, item)
	}
	if len(items) == 1 {
		item := items[0]
		data, err := h.downloadableStickerBytes(c.Request.Context(), item)
		if err != nil {
			h.jsonError(c, http.StatusNotFound, "not_found", "sticker file not found")
			return
		}
		filename := safeDownloadFilename(item.Name, item.Filename)
		c.Header("Content-Disposition", attachmentDisposition(filename))
		c.Data(http.StatusOK, item.MimeType, data)
		return
	}

	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	usedNames := make(map[string]int, len(items))
	for _, item := range items {
		data, err := h.downloadableStickerBytes(c.Request.Context(), item)
		if err != nil {
			_ = archive.Close()
			h.jsonError(c, http.StatusNotFound, "not_found", "sticker file not found")
			return
		}
		entryName := uniqueDownloadFilename(usedNames, safeDownloadFilename(item.Name, item.Filename))
		entry, err := archive.Create(entryName)
		if err != nil {
			_ = archive.Close()
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "create sticker archive failed")
			return
		}
		if _, err := entry.Write(data); err != nil {
			_ = archive.Close()
			h.jsonError(c, http.StatusInternalServerError, "internal_error", "create sticker archive failed")
			return
		}
	}
	if err := archive.Close(); err != nil {
		h.jsonError(c, http.StatusInternalServerError, "internal_error", "create sticker archive failed")
		return
	}
	c.Header("Content-Disposition", attachmentDisposition("stickers.zip"))
	c.Data(http.StatusOK, "application/zip", buffer.Bytes())
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
	name = h.uniqueStickerName(packID, name, "")
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
	h.touchStickerPack(packID)
	c.JSON(http.StatusCreated, gin.H{
		"pack":    h.stickerPackByID(packID),
		"sticker": h.stickerPayload(stickerID),
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

type stickerPackData struct {
	ID        string
	Scope     string
	RoomID    *string
	Name      string
	SortOrder int
	UpdatedAt int64
}

func (h *Handler) stickerPackPayloads(packs []stickerPackData) []gin.H {
	if len(packs) == 0 {
		return []gin.H{}
	}
	packIDs := make([]string, 0, len(packs))
	for _, pack := range packs {
		packIDs = append(packIDs, pack.ID)
	}
	stickersByPack := h.stickersByPackID(packIDs)
	payloads := make([]gin.H, 0, len(packs))
	for _, pack := range packs {
		payloads = append(payloads, stickerPackPayloadWithStickers(pack.ID, pack.Scope, pack.RoomID, pack.Name, pack.SortOrder, pack.UpdatedAt, stickersByPack[pack.ID]))
	}
	return payloads
}

func (h *Handler) stickerPackPayload(id, scope string, roomID *string, name string, sortOrder int, updatedAt int64) gin.H {
	return stickerPackPayloadWithStickers(id, scope, roomID, name, sortOrder, updatedAt, h.stickersForPack(id))
}

func (h *Handler) stickersForPack(packID string) []gin.H {
	rows, _ := h.DB.Query(
		`SELECT s.id, s.name, s.sort_order, a.id, a.url, a.thumbnail_url, a.mime_type, a.width, a.height, a.created_at
		 FROM stickers s JOIN assets a ON a.id = s.asset_id
		 WHERE s.pack_id = ? ORDER BY s.sort_order ASC`,
		packID,
	)
	stickers := make([]gin.H, 0)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var stickerID, stickerName, assetID, url, mimeType string
			var thumb sql.NullString
			var stickerSort int
			var width, height sql.NullInt64
			var createdAt int64
			if err := rows.Scan(&stickerID, &stickerName, &stickerSort, &assetID, &url, &thumb, &mimeType, &width, &height, &createdAt); err == nil {
				stickers = append(stickers, gin.H{
					"id": stickerID, "name": stickerName, "sort_order": stickerSort,
					"asset": gin.H{"id": assetID, "url": url, "thumbnail_url": nullableString(thumb), "mime_type": mimeType, "width": nullableInt64(width), "height": nullableInt64(height), "created_at": formatMillis(createdAt)},
				})
			}
		}
	}
	return stickers
}

func (h *Handler) stickersByPackID(packIDs []string) map[string][]gin.H {
	result := make(map[string][]gin.H, len(packIDs))
	if len(packIDs) == 0 {
		return result
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(packIDs)), ",")
	args := make([]any, 0, len(packIDs))
	for _, packID := range packIDs {
		args = append(args, packID)
	}
	rows, err := h.DB.Query(
		fmt.Sprintf(
			`SELECT s.pack_id, s.id, s.name, s.sort_order, a.id, a.url, a.thumbnail_url, a.mime_type, a.width, a.height, a.created_at
			 FROM stickers s JOIN assets a ON a.id = s.asset_id
			 WHERE s.pack_id IN (%s) ORDER BY s.pack_id ASC, s.sort_order ASC`,
			placeholders,
		),
		args...,
	)
	if err != nil || rows == nil {
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var packID, stickerID, stickerName, assetID, url, mimeType string
		var thumb sql.NullString
		var stickerSort int
		var width, height sql.NullInt64
		var createdAt int64
		if err := rows.Scan(&packID, &stickerID, &stickerName, &stickerSort, &assetID, &url, &thumb, &mimeType, &width, &height, &createdAt); err == nil {
			result[packID] = append(result[packID], gin.H{
				"id": stickerID, "name": stickerName, "sort_order": stickerSort,
				"asset": gin.H{"id": assetID, "url": url, "thumbnail_url": nullableString(thumb), "mime_type": mimeType, "width": nullableInt64(width), "height": nullableInt64(height), "created_at": formatMillis(createdAt)},
			})
		}
	}
	return result
}

func stickerPackPayloadWithStickers(id, scope string, roomID *string, name string, sortOrder int, updatedAt int64, stickers []gin.H) gin.H {
	if stickers == nil {
		stickers = []gin.H{}
	}
	return gin.H{"id": id, "scope": scope, "room_id": roomID, "name": name, "stickers": stickers, "sort_order": sortOrder, "updated_at": formatMillis(updatedAt)}
}

func (h *Handler) stickerPayload(id string) gin.H {
	var stickerID, stickerName, assetID, url, mimeType string
	var thumb sql.NullString
	var stickerSort int
	var width, height sql.NullInt64
	var createdAt int64
	err := h.DB.QueryRow(
		`SELECT s.id, s.name, s.sort_order, a.id, a.url, a.thumbnail_url, a.mime_type, a.width, a.height, a.created_at
		 FROM stickers s JOIN assets a ON a.id = s.asset_id
		 WHERE s.id = ?`,
		id,
	).Scan(&stickerID, &stickerName, &stickerSort, &assetID, &url, &thumb, &mimeType, &width, &height, &createdAt)
	if err != nil {
		return gin.H{"id": id}
	}
	return gin.H{
		"id": stickerID, "name": stickerName, "sort_order": stickerSort,
		"asset": gin.H{"id": assetID, "url": url, "thumbnail_url": nullableString(thumb), "mime_type": mimeType, "width": nullableInt64(width), "height": nullableInt64(height), "created_at": formatMillis(createdAt)},
	}
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

func (h *Handler) uniqueStickerName(packID, desired, excludeStickerID string) string {
	base := strings.TrimSpace(desired)
	if base == "" {
		base = "sticker"
	}
	rows, err := h.DB.Query(`SELECT name FROM stickers WHERE pack_id = ? AND id <> ?`, packID, excludeStickerID)
	if err != nil {
		return base
	}
	defer rows.Close()
	existing := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			existing[name] = true
		}
	}
	if !existing[base] {
		return base
	}
	for index := 2; ; index++ {
		candidate := fmt.Sprintf("%s (%d)", base, index)
		if !existing[candidate] {
			return candidate
		}
	}
}

func (h *Handler) touchStickerPack(packID string) {
	_, _ = h.DB.Exec(`UPDATE sticker_packs SET updated_at = ? WHERE id = ?`, nowMillis(), packID)
}

type downloadableSticker struct {
	ID         string
	Name       string
	AssetID    string
	Filename   string
	MimeType   string
	StorageKey string
}

func (h *Handler) downloadableSticker(stickerID, userID string) (downloadableSticker, bool) {
	var item downloadableSticker
	err := h.DB.QueryRow(
		`SELECT s.id, s.name, a.id, a.filename, a.mime_type, COALESCE(a.storage_key, '')
		 FROM stickers s
		 JOIN sticker_packs p ON p.id = s.pack_id
		 JOIN assets a ON a.id = s.asset_id
		 WHERE s.id = ? AND (
		   (p.scope = 'personal' AND p.owner_user_id = ?)
		   OR (
		     p.scope = 'room'
		     AND (
		       EXISTS (SELECT 1 FROM room_memberships rm WHERE rm.room_id = p.room_id AND rm.user_id = ?)
		       OR ? = 1
		     )
		   )
		 )`,
		stickerID, userID, userID, boolToInt(h.isSuperuser(userID)),
	).Scan(&item.ID, &item.Name, &item.AssetID, &item.Filename, &item.MimeType, &item.StorageKey)
	return item, err == nil
}

func (h *Handler) downloadableStickerBytes(ctx context.Context, item downloadableSticker) ([]byte, error) {
	assetStore := h.assetStore()
	storageKey := item.StorageKey
	if storageKey == "" {
		storageKey = assetStore.ObjectKey(item.AssetID, item.Filename)
	}
	body, err := assetStore.Open(ctx, storageKey, item.AssetID, item.Filename)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

func parseStickerIDList(raw string) []string {
	seen := map[string]bool{}
	ids := make([]string, 0)
	for _, value := range strings.Split(raw, ",") {
		id := strings.TrimSpace(value)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

func safeDownloadFilename(name, fallback string) string {
	ext := filepath.Ext(fallback)
	base := strings.TrimSpace(name)
	if base == "" {
		base = strings.TrimSuffix(filepath.Base(fallback), filepath.Ext(fallback))
	}
	var cleaned strings.Builder
	for _, r := range base {
		if r < 32 || strings.ContainsRune(`\/:*?"<>|`, r) {
			cleaned.WriteByte('-')
			continue
		}
		cleaned.WriteRune(r)
	}
	value := strings.Trim(cleaned.String(), " .-_")
	if value == "" {
		value = "sticker"
	}
	if ext == "" {
		ext = ".png"
	}
	return value + strings.ToLower(ext)
}

func uniqueDownloadFilename(used map[string]int, filename string) string {
	if used[filename] == 0 {
		used[filename] = 1
		return filename
	}
	used[filename]++
	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	return fmt.Sprintf("%s (%d)%s", stem, used[filename], ext)
}

func attachmentDisposition(filename string) string {
	return `attachment; filename="` + dispositionFallback(filename) + `"; filename*=UTF-8''` + url.PathEscape(filename)
}

func dispositionFallback(filename string) string {
	var out strings.Builder
	for _, r := range filename {
		if r < 32 || r > 126 || strings.ContainsRune(`\";`, r) {
			out.WriteByte('_')
			continue
		}
		out.WriteRune(r)
	}
	value := strings.TrimSpace(out.String())
	if value == "" {
		return "download"
	}
	return value
}
