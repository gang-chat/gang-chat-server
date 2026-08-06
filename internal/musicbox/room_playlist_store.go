package musicbox

import (
	"context"
	"database/sql"
	"strings"
)

// ListRoomPlaylists returns only playlists owned by roomID. Room membership
// and management permissions are intentionally enforced by the chat API; the
// store still scopes every query by room to prevent cross-room mutations.
func (s *PlaylistStore) ListRoomPlaylists(
	ctx context.Context,
	roomID string,
	page, pageSize int,
) (PlaylistPage, error) {
	page, pageSize = normalizePage(page, pageSize)
	var total int
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM music_playlists
		 WHERE scope_type = 'room' AND room_id = ?`,
		roomID,
	).Scan(&total); err != nil {
		return PlaylistPage{}, err
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT p.id, p.name, p.description, p.revision, COUNT(i.id),
		        p.created_at, p.updated_at
		 FROM music_playlists p
		 LEFT JOIN music_playlist_items i ON i.playlist_id = p.id
		 WHERE p.scope_type = 'room' AND p.room_id = ?
		 GROUP BY p.id, p.name, p.description, p.revision, p.sort_order,
		          p.created_at, p.updated_at
		 ORDER BY p.sort_order ASC, p.created_at ASC, p.id ASC
		 LIMIT ? OFFSET ?`,
		roomID,
		pageSize,
		(page-1)*pageSize,
	)
	if err != nil {
		return PlaylistPage{}, err
	}
	defer rows.Close()
	items := make([]PlaylistSummary, 0, pageSize)
	for rows.Next() {
		var item PlaylistSummary
		if err := rows.Scan(
			&item.ID,
			&item.Name,
			&item.Description,
			&item.Revision,
			&item.ItemCount,
			&item.CreatedAt,
			&item.UpdatedAt,
		); err != nil {
			return PlaylistPage{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return PlaylistPage{}, err
	}
	return PlaylistPage{
		Items:    items,
		Page:     page,
		PageSize: pageSize,
		Total:    total,
		HasMore:  page*pageSize < total,
	}, nil
}

func (s *PlaylistStore) CreateRoomPlaylist(
	ctx context.Context,
	roomID, name string,
) (PlaylistSummary, error) {
	return s.createRoomPlaylist(ctx, roomID, name, "", "")
}

// CreateRoomPlaylistFromUserPlaylist creates a room playlist and copies the
// current user's saved playlist into it in the same transaction. Locking the
// source playlist keeps the imported order stable while another request edits
// that personal playlist, and every target item receives a fresh identity.
func (s *PlaylistStore) CreateRoomPlaylistFromUserPlaylist(
	ctx context.Context,
	roomID, name, ownerUserID, sourcePlaylistID string,
) (PlaylistSummary, error) {
	if strings.TrimSpace(ownerUserID) == "" || strings.TrimSpace(sourcePlaylistID) == "" {
		return PlaylistSummary{}, ErrPlaylistNotFound
	}
	return s.createRoomPlaylist(
		ctx,
		roomID,
		name,
		ownerUserID,
		sourcePlaylistID,
	)
}

func (s *PlaylistStore) createRoomPlaylist(
	ctx context.Context,
	roomID, name, sourceOwnerUserID, sourcePlaylistID string,
) (PlaylistSummary, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlaylistSummary{}, err
	}
	defer tx.Rollback()
	if err := lockRoomPlaylistNamespace(ctx, tx, roomID); err != nil {
		return PlaylistSummary{}, err
	}
	importedTrackIDs := make([]string, 0)
	if sourcePlaylistID != "" {
		if _, err := s.playlistSummary(
			ctx,
			tx,
			userPlaylistScope(sourceOwnerUserID),
			sourcePlaylistID,
			true,
		); err != nil {
			return PlaylistSummary{}, err
		}
		rows, err := tx.QueryContext(
			ctx,
			`SELECT track_id FROM music_playlist_items
			 WHERE playlist_id = ?
			 ORDER BY sort_order ASC, created_at ASC, id ASC
			 FOR UPDATE`,
			sourcePlaylistID,
		)
		if err != nil {
			return PlaylistSummary{}, err
		}
		for rows.Next() {
			var trackID string
			if err := rows.Scan(&trackID); err != nil {
				rows.Close()
				return PlaylistSummary{}, err
			}
			importedTrackIDs = append(importedTrackIDs, trackID)
			if len(importedTrackIDs) > MaxPlaylistItems {
				rows.Close()
				return PlaylistSummary{}, ErrPlaylistItemLimit
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return PlaylistSummary{}, err
		}
		if err := rows.Close(); err != nil {
			return PlaylistSummary{}, err
		}
	}
	var count int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM music_playlists
		 WHERE scope_type = 'room' AND room_id = ?`,
		roomID,
	).Scan(&count); err != nil {
		return PlaylistSummary{}, err
	}
	if count >= MaxRoomPlaylists {
		return PlaylistSummary{}, ErrPlaylistLimit
	}
	if exists, err := roomPlaylistNameExists(ctx, tx, roomID, name, ""); err != nil {
		return PlaylistSummary{}, err
	} else if exists {
		return PlaylistSummary{}, ErrPlaylistName
	}
	var maxOrder sql.NullInt64
	if err := tx.QueryRowContext(
		ctx,
		`SELECT MAX(sort_order) FROM music_playlists
		 WHERE scope_type = 'room' AND room_id = ?`,
		roomID,
	).Scan(&maxOrder); err != nil {
		return PlaylistSummary{}, err
	}
	sortOrder := int64(10)
	if maxOrder.Valid {
		sortOrder = maxOrder.Int64 + 10
	}
	now := nowMillis()
	playlist := PlaylistSummary{
		ID:        "mbp_" + randomID(),
		Name:      name,
		Revision:  1,
		ItemCount: len(importedTrackIDs),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO music_playlists
		 (id, scope_type, owner_user_id, room_id, name, description,
		  revision, sort_order, created_at, updated_at)
		 VALUES (?, 'room', NULL, ?, ?, '', 1, ?, ?, ?)`,
		playlist.ID,
		roomID,
		playlist.Name,
		sortOrder,
		now,
		now,
	); err != nil {
		return PlaylistSummary{}, err
	}
	for index, trackID := range importedTrackIDs {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO music_playlist_items
			 (id, playlist_id, track_id, added_by_user_id, sort_order,
			  created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"mbpi_"+randomID(),
			playlist.ID,
			trackID,
			sourceOwnerUserID,
			int64(index+1)*10,
			now,
			now,
		); err != nil {
			return PlaylistSummary{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return PlaylistSummary{}, err
	}
	return playlist, nil
}

func (s *PlaylistStore) DeleteRoomPlaylist(
	ctx context.Context,
	roomID, playlistID string,
) (bool, error) {
	result, err := s.db.ExecContext(
		ctx,
		`DELETE FROM music_playlists
		 WHERE id = ? AND scope_type = 'room' AND room_id = ?`,
		playlistID,
		roomID,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

func (s *PlaylistStore) RenameRoomPlaylist(
	ctx context.Context,
	roomID, playlistID, name string,
) (PlaylistSummary, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlaylistSummary{}, err
	}
	defer tx.Rollback()
	if err := lockRoomPlaylistNamespace(ctx, tx, roomID); err != nil {
		return PlaylistSummary{}, err
	}
	if _, err := s.playlistSummary(
		ctx,
		tx,
		roomPlaylistScope(roomID),
		playlistID,
		true,
	); err != nil {
		return PlaylistSummary{}, err
	}
	if exists, err := roomPlaylistNameExists(
		ctx,
		tx,
		roomID,
		name,
		playlistID,
	); err != nil {
		return PlaylistSummary{}, err
	} else if exists {
		return PlaylistSummary{}, ErrPlaylistName
	}
	now := nowMillis()
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE music_playlists
		 SET name = ?, revision = revision + 1, updated_at = ?
		 WHERE id = ? AND scope_type = 'room' AND room_id = ?`,
		name,
		now,
		playlistID,
		roomID,
	); err != nil {
		return PlaylistSummary{}, err
	}
	playlist, err := s.playlistSummary(
		ctx,
		tx,
		roomPlaylistScope(roomID),
		playlistID,
		false,
	)
	if err != nil {
		return PlaylistSummary{}, err
	}
	if err := tx.Commit(); err != nil {
		return PlaylistSummary{}, err
	}
	return playlist, nil
}

// PinRoomPlaylists preserves the selection order and the relative order of
// every unselected room playlist.
func (s *PlaylistStore) PinRoomPlaylists(
	ctx context.Context,
	roomID string,
	playlistIDs []string,
) error {
	if len(playlistIDs) == 0 || len(playlistIDs) > MaxRoomPlaylists {
		return ErrPlaylistOrder
	}
	selected := make(map[string]struct{}, len(playlistIDs))
	for _, playlistID := range playlistIDs {
		playlistID = strings.TrimSpace(playlistID)
		if playlistID == "" {
			return ErrPlaylistOrder
		}
		if _, exists := selected[playlistID]; exists {
			return ErrPlaylistOrder
		}
		selected[playlistID] = struct{}{}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockRoomPlaylistNamespace(ctx, tx, roomID); err != nil {
		return err
	}
	currentOrder, err := lockedRoomPlaylistIDs(ctx, tx, roomID)
	if err != nil {
		return err
	}
	for playlistID := range selected {
		if !containsString(currentOrder, playlistID) {
			return ErrPlaylistOrder
		}
	}
	nextOrder := make([]string, 0, len(currentOrder))
	nextOrder = append(nextOrder, playlistIDs...)
	for _, playlistID := range currentOrder {
		if _, pinned := selected[playlistID]; !pinned {
			nextOrder = append(nextOrder, playlistID)
		}
	}
	if sameStringList(currentOrder, nextOrder) {
		return tx.Commit()
	}
	if err := updateRoomPlaylistOrder(ctx, tx, roomID, nextOrder); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PlaylistStore) MoveRoomPlaylist(
	ctx context.Context,
	roomID, playlistID string,
	direction int,
) error {
	if direction != -1 && direction != 1 {
		return ErrPlaylistOrder
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockRoomPlaylistNamespace(ctx, tx, roomID); err != nil {
		return err
	}
	playlistIDs, err := lockedRoomPlaylistIDs(ctx, tx, roomID)
	if err != nil {
		return err
	}
	index := -1
	for candidateIndex, candidateID := range playlistIDs {
		if candidateID == playlistID {
			index = candidateIndex
			break
		}
	}
	if index < 0 {
		return ErrPlaylistNotFound
	}
	target := index + direction
	if target < 0 || target >= len(playlistIDs) {
		return ErrPlaylistOrder
	}
	playlistIDs[index], playlistIDs[target] = playlistIDs[target], playlistIDs[index]
	if err := updateRoomPlaylistOrder(ctx, tx, roomID, playlistIDs); err != nil {
		return err
	}
	return tx.Commit()
}

func lockRoomPlaylistNamespace(
	ctx context.Context,
	tx *sql.Tx,
	roomID string,
) error {
	var lockedRoomID string
	err := tx.QueryRowContext(
		ctx,
		`SELECT id FROM rooms WHERE id = ? FOR UPDATE`,
		roomID,
	).Scan(&lockedRoomID)
	if err == sql.ErrNoRows {
		return ErrPlaylistNotFound
	}
	return err
}

func roomPlaylistNameExists(
	ctx context.Context,
	tx *sql.Tx,
	roomID, name, excludePlaylistID string,
) (bool, error) {
	query := `SELECT COUNT(*) FROM music_playlists
	          WHERE scope_type = 'room' AND room_id = ? AND name = ?`
	args := []any{roomID, name}
	if excludePlaylistID != "" {
		query += " AND id != ?"
		args = append(args, excludePlaylistID)
	}
	var count int
	err := tx.QueryRowContext(ctx, query, args...).Scan(&count)
	return count != 0, err
}

func lockedRoomPlaylistIDs(
	ctx context.Context,
	tx *sql.Tx,
	roomID string,
) ([]string, error) {
	rows, err := tx.QueryContext(
		ctx,
		`SELECT id FROM music_playlists
		 WHERE scope_type = 'room' AND room_id = ?
		 ORDER BY sort_order ASC, created_at ASC, id ASC
		 FOR UPDATE`,
		roomID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	playlistIDs := make([]string, 0, MaxRoomPlaylists)
	for rows.Next() {
		var playlistID string
		if err := rows.Scan(&playlistID); err != nil {
			return nil, err
		}
		playlistIDs = append(playlistIDs, playlistID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return playlistIDs, nil
}

func updateRoomPlaylistOrder(
	ctx context.Context,
	tx *sql.Tx,
	roomID string,
	playlistIDs []string,
) error {
	now := nowMillis()
	for index, playlistID := range playlistIDs {
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE music_playlists
			 SET sort_order = ?, updated_at = ?
			 WHERE id = ? AND scope_type = 'room' AND room_id = ?`,
			(index+1)*10,
			now,
			playlistID,
			roomID,
		); err != nil {
			return err
		}
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
