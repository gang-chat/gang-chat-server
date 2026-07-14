package chat

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
)

const (
	stickerAssetCleanupBatchSize = 100
)

// ensureStickerAssetLifecycleSchema creates the small amount of durable state
// needed to expire orphaned sticker uploads. References remain authoritative in
// their owning tables (messages, stickers, avatars); the lifecycle table only
// records when an otherwise-unreferenced asset becomes eligible for cleanup.
func (h *Handler) ensureStickerAssetLifecycleSchema() error {
	if _, err := h.DB.Exec(
		`CREATE TABLE IF NOT EXISTS sticker_asset_lifecycle (
			asset_id VARCHAR(128) NOT NULL,
			expires_at BIGINT NULL,
			updated_at BIGINT NOT NULL,
			PRIMARY KEY (asset_id),
			INDEX idx_sticker_asset_lifecycle_expires_at (expires_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	); err != nil {
		return err
	}

	rows, err := h.DB.Query(
		`SELECT a.id
		 FROM assets a
		 LEFT JOIN sticker_asset_lifecycle lifecycle ON lifecycle.asset_id = a.id
		 WHERE a.purpose = 'sticker' AND lifecycle.asset_id IS NULL`,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	assetIDs := make([]string, 0)
	for rows.Next() {
		var assetID string
		if err := rows.Scan(&assetID); err != nil {
			return err
		}
		assetIDs = append(assetIDs, assetID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, assetID := range assetIDs {
		if err := h.scheduleStickerAssetExpiry(assetID); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) stickerAssetOrphanTTL() time.Duration {
	if h.Cfg != nil && h.Cfg.StickerAssetOrphanTTLSeconds > 0 {
		return time.Duration(h.Cfg.StickerAssetOrphanTTLSeconds) * time.Second
	}
	return time.Duration(config.DefaultStickerAssetOrphanTTLSeconds) * time.Second
}

func (h *Handler) stickerAssetCleanupInterval() time.Duration {
	if h.Cfg != nil && h.Cfg.StickerAssetCleanupIntervalSeconds > 0 {
		return time.Duration(h.Cfg.StickerAssetCleanupIntervalSeconds) * time.Second
	}
	return time.Duration(config.DefaultStickerAssetCleanupIntervalSeconds) * time.Second
}

func (h *Handler) trackUploadedStickerAsset(assetID, purpose string) error {
	if strings.TrimSpace(purpose) != "sticker" {
		return nil
	}
	return h.setStickerAssetExpiry(assetID, nowMillis()+h.stickerAssetOrphanTTL().Milliseconds())
}

func (h *Handler) retainStickerAsset(assetID string) error {
	assetID = strings.TrimSpace(assetID)
	if assetID == "" {
		return nil
	}
	_, err := h.DB.Exec(
		`INSERT INTO sticker_asset_lifecycle (asset_id, expires_at, updated_at)
		 SELECT id, NULL, ? FROM assets WHERE id = ? AND purpose = 'sticker'
		 ON DUPLICATE KEY UPDATE expires_at = NULL, updated_at = VALUES(updated_at)`,
		nowMillis(), assetID,
	)
	return err
}

func (h *Handler) setStickerAssetExpiry(assetID string, expiresAt int64) error {
	assetID = strings.TrimSpace(assetID)
	if assetID == "" {
		return nil
	}
	_, err := h.DB.Exec(
		`INSERT INTO sticker_asset_lifecycle (asset_id, expires_at, updated_at)
		 SELECT id, ?, ? FROM assets WHERE id = ? AND purpose = 'sticker'
		 ON DUPLICATE KEY UPDATE expires_at = VALUES(expires_at), updated_at = VALUES(updated_at)`,
		expiresAt, nowMillis(), assetID,
	)
	return err
}

func (h *Handler) scheduleStickerAssetExpiry(assetID string) error {
	assetID = strings.TrimSpace(assetID)
	if assetID == "" {
		return nil
	}
	unlock := h.assetLifecycleLocks.lock(assetID)
	defer unlock()

	referenced, err := h.stickerAssetReferenced(assetID)
	if err != nil {
		return err
	}
	if referenced {
		return h.retainStickerAsset(assetID)
	}
	return h.setStickerAssetExpiry(
		assetID,
		nowMillis()+h.stickerAssetOrphanTTL().Milliseconds(),
	)
}

// stickerAssetReferenced derives liveness from the owning records instead of
// maintaining a second reference-count table that could drift after crashes.
func (h *Handler) stickerAssetReferenced(assetID string) (bool, error) {
	assetID = strings.TrimSpace(assetID)
	if assetID == "" {
		return false, nil
	}
	assetURLNeedle := "/assets/" + assetID + "/"
	checks := []struct {
		query string
		args  []any
	}{
		{
			query: `SELECT EXISTS(SELECT 1 FROM stickers WHERE asset_id = ? LIMIT 1)`,
			args:  []any{assetID},
		},
		{
			query: `SELECT EXISTS(
				SELECT 1
				FROM messages m
				WHERE EXISTS (
					SELECT 1
					FROM JSON_TABLE(m.attachments_json, '$[*]' COLUMNS (
						asset_id VARCHAR(128) PATH '$.asset.id' NULL ON EMPTY
					)) attachment
					WHERE attachment.asset_id = ?
				)
				OR LOCATE(?, m.attachments_json) > 0
				OR LOCATE(?, COALESCE(m.sender_avatar_url_snapshot, '')) > 0
				LIMIT 1
			)`,
			args: []any{assetID, assetURLNeedle, assetURLNeedle},
		},
		{
			query: `SELECT EXISTS(SELECT 1 FROM users WHERE LOCATE(?, COALESCE(avatar_url, '')) > 0 LIMIT 1)`,
			args:  []any{assetURLNeedle},
		},
		{
			query: `SELECT EXISTS(
				SELECT 1 FROM rooms
				WHERE avatar_asset_id = ? OR LOCATE(?, COALESCE(avatar_url, '')) > 0
				LIMIT 1
			)`,
			args: []any{assetID, assetURLNeedle},
		},
		{
			query: `SELECT EXISTS(SELECT 1 FROM room_invites WHERE LOCATE(?, COALESCE(room_avatar_url, '')) > 0 LIMIT 1)`,
			args:  []any{assetURLNeedle},
		},
		{
			query: `SELECT EXISTS(
				SELECT 1 FROM room_notifications
				WHERE LOCATE(?, COALESCE(room_avatar_url, '')) > 0
				   OR LOCATE(?, COALESCE(actor_avatar_url, '')) > 0
				LIMIT 1
			)`,
			args: []any{assetURLNeedle, assetURLNeedle},
		},
	}
	for _, check := range checks {
		var exists int
		if err := h.DB.QueryRow(check.query, check.args...).Scan(&exists); err != nil {
			return false, err
		}
		if exists != 0 {
			return true, nil
		}
	}
	return false, nil
}

// RunStickerAssetCleanup periodically removes expired sticker resources. Each
// candidate is checked again immediately before deletion so a newly-created
// message or pack reference always wins over cleanup.
func (h *Handler) RunStickerAssetCleanup(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := h.cleanupExpiredStickerAssets(ctx); err != nil {
		log.Printf("chat: initial sticker asset cleanup: %v", err)
	}
	ticker := time.NewTicker(h.stickerAssetCleanupInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := h.cleanupExpiredStickerAssets(ctx); err != nil {
				log.Printf("chat: sticker asset cleanup: %v", err)
			}
		}
	}
}

func (h *Handler) cleanupExpiredStickerAssets(ctx context.Context) error {
	if err := h.reconcileUnreferencedStickerAssets(ctx); err != nil {
		return err
	}
	rows, err := h.DB.QueryContext(
		ctx,
		`SELECT lifecycle.asset_id
		 FROM sticker_asset_lifecycle lifecycle
		 JOIN assets a ON a.id = lifecycle.asset_id AND a.purpose = 'sticker'
		 WHERE lifecycle.expires_at IS NOT NULL AND lifecycle.expires_at <= ?
		 ORDER BY lifecycle.expires_at ASC
		 LIMIT ?`,
		nowMillis(), stickerAssetCleanupBatchSize,
	)
	if err != nil {
		return err
	}
	assetIDs := make([]string, 0)
	for rows.Next() {
		var assetID string
		if err := rows.Scan(&assetID); err != nil {
			rows.Close()
			return err
		}
		assetIDs = append(assetIDs, assetID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, assetID := range assetIDs {
		if err := h.cleanupExpiredStickerAsset(ctx, assetID); err != nil {
			return err
		}
	}
	return nil
}

// reconcileUnreferencedStickerAssets is the crash/cascade safety net. Normal
// mutation paths schedule expiry immediately, while this query catches rows
// removed indirectly by room/account cascades or by an interrupted request.
func (h *Handler) reconcileUnreferencedStickerAssets(ctx context.Context) error {
	now := nowMillis()
	_, err := h.DB.ExecContext(
		ctx,
		`UPDATE sticker_asset_lifecycle lifecycle
		 JOIN assets a ON a.id = lifecycle.asset_id AND a.purpose = 'sticker'
		 SET lifecycle.expires_at = ?, lifecycle.updated_at = ?
		 WHERE lifecycle.expires_at IS NULL
		   AND NOT EXISTS (SELECT 1 FROM stickers s WHERE s.asset_id = lifecycle.asset_id)
		   AND NOT EXISTS (
		     SELECT 1
		     FROM messages m
		     WHERE EXISTS (
		       SELECT 1
		       FROM JSON_TABLE(m.attachments_json, '$[*]' COLUMNS (
		         asset_id VARCHAR(128) PATH '$.asset.id' NULL ON EMPTY
		       )) attachment
		       WHERE attachment.asset_id = lifecycle.asset_id
		     )
		     OR LOCATE(CONCAT('/assets/', lifecycle.asset_id, '/'), m.attachments_json) > 0
		     OR LOCATE(CONCAT('/assets/', lifecycle.asset_id, '/'), COALESCE(m.sender_avatar_url_snapshot, '')) > 0
		   )
		   AND NOT EXISTS (
		     SELECT 1 FROM users u
		     WHERE LOCATE(CONCAT('/assets/', lifecycle.asset_id, '/'), COALESCE(u.avatar_url, '')) > 0
		   )
		   AND NOT EXISTS (
		     SELECT 1 FROM rooms r
		     WHERE r.avatar_asset_id = lifecycle.asset_id
		        OR LOCATE(CONCAT('/assets/', lifecycle.asset_id, '/'), COALESCE(r.avatar_url, '')) > 0
		   )
		   AND NOT EXISTS (
		     SELECT 1 FROM room_invites invite
		     WHERE LOCATE(CONCAT('/assets/', lifecycle.asset_id, '/'), COALESCE(invite.room_avatar_url, '')) > 0
		   )
		   AND NOT EXISTS (
		     SELECT 1 FROM room_notifications notification
		     WHERE LOCATE(CONCAT('/assets/', lifecycle.asset_id, '/'), COALESCE(notification.room_avatar_url, '')) > 0
		        OR LOCATE(CONCAT('/assets/', lifecycle.asset_id, '/'), COALESCE(notification.actor_avatar_url, '')) > 0
		   )`,
		now+h.stickerAssetOrphanTTL().Milliseconds(), now,
	)
	return err
}

func (h *Handler) cleanupExpiredStickerAsset(ctx context.Context, assetID string) error {
	unlock := h.assetLifecycleLocks.lock(assetID)
	defer unlock()

	var expiresAt sql.NullInt64
	if err := h.DB.QueryRowContext(
		ctx,
		`SELECT expires_at FROM sticker_asset_lifecycle WHERE asset_id = ?`,
		assetID,
	).Scan(&expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if !expiresAt.Valid || expiresAt.Int64 > nowMillis() {
		return nil
	}
	referenced, err := h.stickerAssetReferenced(assetID)
	if err != nil {
		return err
	}
	if referenced {
		return h.retainStickerAsset(assetID)
	}

	var filename string
	var storageKey sql.NullString
	if err := h.DB.QueryRowContext(
		ctx,
		`SELECT filename, storage_key FROM assets WHERE id = ? AND purpose = 'sticker'`,
		assetID,
	).Scan(&filename, &storageKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_, cleanupErr := h.DB.ExecContext(ctx, `DELETE FROM sticker_asset_lifecycle WHERE asset_id = ?`, assetID)
			return cleanupErr
		}
		return err
	}
	key := storageKey.String
	if key == "" {
		key = h.Assets.ObjectKey(assetID, filename)
	}
	if err := h.Assets.Delete(ctx, key); err != nil {
		return err
	}
	if _, err := h.DB.ExecContext(ctx, `DELETE FROM assets WHERE id = ? AND purpose = 'sticker'`, assetID); err != nil {
		return err
	}
	_, err = h.DB.ExecContext(ctx, `DELETE FROM sticker_asset_lifecycle WHERE asset_id = ?`, assetID)
	return err
}

func (h *Handler) stickerAssetAvailable(ctx context.Context, assetID string) bool {
	var filename, purpose string
	var storageKey sql.NullString
	if err := h.DB.QueryRowContext(
		ctx,
		`SELECT filename, purpose, storage_key FROM assets WHERE id = ?`,
		assetID,
	).Scan(&filename, &purpose, &storageKey); err != nil || purpose != "sticker" {
		return false
	}
	key := storageKey.String
	if key == "" {
		key = h.Assets.ObjectKey(assetID, filename)
	}
	body, err := h.Assets.Open(ctx, key, assetID, filename)
	if err != nil {
		return false
	}
	return body.Close() == nil
}

func stickerAssetFromAttachments(attachments []any, stickerID string) (string, string, bool) {
	for _, raw := range attachments {
		attachment, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(stringValue(attachment["type"])) != "sticker" {
			continue
		}
		if strings.TrimSpace(stringValue(attachment["sticker_id"])) != stickerID {
			continue
		}
		asset, ok := attachment["asset"].(map[string]any)
		if !ok {
			continue
		}
		assetID := strings.TrimSpace(stringValue(asset["id"]))
		if assetID == "" {
			continue
		}
		return assetID, strings.TrimSpace(stringValue(attachment["name"])), true
	}
	return "", "", false
}

func stickerAssetIDsFromAttachments(attachments []any) []string {
	seen := make(map[string]bool)
	assetIDs := make([]string, 0)
	for _, raw := range attachments {
		attachment, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(stringValue(attachment["type"])) != "sticker" {
			continue
		}
		asset, ok := attachment["asset"].(map[string]any)
		if !ok {
			continue
		}
		assetID := strings.TrimSpace(stringValue(asset["id"]))
		if assetID == "" || seen[assetID] {
			continue
		}
		seen[assetID] = true
		assetIDs = append(assetIDs, assetID)
	}
	return assetIDs
}

func (h *Handler) messageStickerAssetIDs(messageID string) []string {
	var raw string
	if err := h.DB.QueryRow(`SELECT attachments_json FROM messages WHERE id = ?`, messageID).Scan(&raw); err != nil {
		return nil
	}
	return stickerAssetIDsFromAttachments(decodeJSONArray(raw))
}

func (h *Handler) stickerAssetFromMessage(roomID, messageID, stickerID string) (string, string, bool) {
	var raw string
	if err := h.DB.QueryRow(
		`SELECT attachments_json
		 FROM messages
		 WHERE id = ? AND room_id = ? AND is_recalled = 0 AND is_force_deleted = 0`,
		messageID, roomID,
	).Scan(&raw); err != nil {
		return "", "", false
	}
	return stickerAssetFromAttachments(decodeJSONArray(raw), stickerID)
}

func (h *Handler) stickerAssetIDsForPack(packID string) []string {
	rows, err := h.DB.Query(`SELECT DISTINCT asset_id FROM stickers WHERE pack_id = ?`, packID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	assetIDs := make([]string, 0)
	for rows.Next() {
		var assetID string
		if err := rows.Scan(&assetID); err == nil && assetID != "" {
			assetIDs = append(assetIDs, assetID)
		}
	}
	return assetIDs
}

func (h *Handler) stickerAssetIDsForRoom(roomID string) []string {
	seen := make(map[string]bool)
	assetIDs := make([]string, 0)
	appendAssetID := func(assetID string) {
		assetID = strings.TrimSpace(assetID)
		if assetID == "" || seen[assetID] {
			return
		}
		seen[assetID] = true
		assetIDs = append(assetIDs, assetID)
	}

	rows, err := h.DB.Query(`SELECT attachments_json FROM messages WHERE room_id = ?`, roomID)
	if err == nil {
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				continue
			}
			for _, assetID := range stickerAssetIDsFromAttachments(decodeJSONArray(raw)) {
				appendAssetID(assetID)
			}
		}
		rows.Close()
	}

	rows, err = h.DB.Query(
		`SELECT DISTINCT s.asset_id
		 FROM stickers s
		 JOIN sticker_packs p ON p.id = s.pack_id
		 WHERE p.scope = 'room' AND p.room_id = ?`,
		roomID,
	)
	if err == nil {
		for rows.Next() {
			var assetID string
			if err := rows.Scan(&assetID); err == nil {
				appendAssetID(assetID)
			}
		}
		rows.Close()
	}
	return assetIDs
}

func (h *Handler) scheduleStickerAssets(assetIDs []string) {
	for _, assetID := range assetIDs {
		if err := h.scheduleStickerAssetExpiry(assetID); err != nil {
			log.Printf("chat: schedule sticker asset %s expiry: %v", assetID, err)
		}
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
