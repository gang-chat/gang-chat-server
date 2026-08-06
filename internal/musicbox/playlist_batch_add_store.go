package musicbox

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type playlistBatchAddSelection struct {
	targetTrackIDs      []string
	selectedItemCount   int
	uniqueItemCount     int
	duplicateCount      int
	alreadyPresentCount int
	omittedCount        int
}

// BatchAddUserPlaylistItems copies selected items between personal playlists.
// The source playlist is never mutated.
func (s *PlaylistStore) BatchAddUserPlaylistItems(
	ctx context.Context,
	ownerUserID, sourcePlaylistID, targetPlaylistID string,
	itemIDs []string,
) (PlaylistBatchAddResult, error) {
	return s.batchAddPlaylistItems(
		ctx,
		userPlaylistScope(ownerUserID),
		ownerUserID,
		sourcePlaylistID,
		targetPlaylistID,
		itemIDs,
	)
}

// BatchAddRoomPlaylistItems applies the same rule between playlists in one
// room. The actor is recorded as the adder of copied target items.
func (s *PlaylistStore) BatchAddRoomPlaylistItems(
	ctx context.Context,
	roomID, actorUserID, sourcePlaylistID, targetPlaylistID string,
	itemIDs []string,
) (PlaylistBatchAddResult, error) {
	return s.batchAddPlaylistItems(
		ctx,
		roomPlaylistScope(roomID),
		actorUserID,
		sourcePlaylistID,
		targetPlaylistID,
		itemIDs,
	)
}

func (s *PlaylistStore) batchAddPlaylistItems(
	ctx context.Context,
	scope playlistScope,
	actorUserID, sourcePlaylistID, targetPlaylistID string,
	itemIDs []string,
) (PlaylistBatchAddResult, error) {
	itemIDs, ok := normalizedBatchAddItemIDs(itemIDs)
	if !ok || strings.TrimSpace(sourcePlaylistID) == "" ||
		strings.TrimSpace(targetPlaylistID) == "" ||
		sourcePlaylistID == targetPlaylistID {
		return PlaylistBatchAddResult{}, ErrPlaylistSelection
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlaylistBatchAddResult{}, err
	}
	defer tx.Rollback()

	if scope.kind == "room" {
		if err := lockRoomPlaylistNamespace(ctx, tx, scope.roomID); err != nil {
			return PlaylistBatchAddResult{}, err
		}
	} else {
		var lockedUserID string
		err := tx.QueryRowContext(
			ctx,
			`SELECT id FROM users WHERE id = ? FOR UPDATE`,
			scope.ownerUserID,
		).Scan(&lockedUserID)
		if errors.Is(err, sql.ErrNoRows) {
			return PlaylistBatchAddResult{}, ErrPlaylistNotFound
		}
		if err != nil {
			return PlaylistBatchAddResult{}, err
		}
	}

	if _, err := s.playlistSummary(ctx, tx, scope, sourcePlaylistID, true); err != nil {
		return PlaylistBatchAddResult{}, err
	}
	target, err := s.playlistSummary(ctx, tx, scope, targetPlaylistID, true)
	if err != nil {
		return PlaylistBatchAddResult{}, err
	}

	sourceTracks, err := lockedPlaylistTracks(ctx, tx, sourcePlaylistID)
	if err != nil {
		return PlaylistBatchAddResult{}, err
	}
	tracksByItemID := make(map[string]playlistMergeTrack, len(sourceTracks))
	for _, track := range sourceTracks {
		tracksByItemID[track.itemID] = track
	}
	selectedTracks := make([]playlistMergeTrack, 0, len(itemIDs))
	for _, itemID := range itemIDs {
		track, exists := tracksByItemID[itemID]
		if !exists {
			return PlaylistBatchAddResult{}, ErrPlaylistSelection
		}
		selectedTracks = append(selectedTracks, track)
	}

	targetTracks, err := lockedPlaylistTracks(ctx, tx, targetPlaylistID)
	if err != nil {
		return PlaylistBatchAddResult{}, err
	}
	existingLinks := make(map[string]struct{}, len(targetTracks))
	for _, track := range targetTracks {
		existingLinks[playlistMergeLinkKey(track)] = struct{}{}
	}
	selection := selectBatchAddTracks(
		selectedTracks,
		existingLinks,
		MaxPlaylistItems-len(targetTracks),
	)

	var maxOrder sql.NullInt64
	if err := tx.QueryRowContext(
		ctx,
		`SELECT MAX(sort_order) FROM music_playlist_items WHERE playlist_id = ?`,
		targetPlaylistID,
	).Scan(&maxOrder); err != nil {
		return PlaylistBatchAddResult{}, err
	}
	nextOrder := int64(10)
	if maxOrder.Valid {
		nextOrder = maxOrder.Int64 + 10
	}
	now := nowMillis()
	for index, trackID := range selection.targetTrackIDs {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO music_playlist_items
			 (id, playlist_id, track_id, added_by_user_id, sort_order,
			  created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"mbpi_"+randomID(),
			targetPlaylistID,
			trackID,
			actorUserID,
			nextOrder+int64(index)*10,
			now,
			now,
		); err != nil {
			return PlaylistBatchAddResult{}, err
		}
	}
	if len(selection.targetTrackIDs) != 0 {
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE music_playlists
			 SET revision = revision + 1, updated_at = ? WHERE id = ?`,
			now,
			targetPlaylistID,
		); err != nil {
			return PlaylistBatchAddResult{}, err
		}
		target.Revision++
		target.UpdatedAt = now
		target.ItemCount += len(selection.targetTrackIDs)
	}
	if err := tx.Commit(); err != nil {
		return PlaylistBatchAddResult{}, err
	}
	return PlaylistBatchAddResult{
		Playlist:            target,
		SelectedItemCount:   selection.selectedItemCount,
		UniqueItemCount:     selection.uniqueItemCount,
		DuplicateCount:      selection.duplicateCount,
		AlreadyPresentCount: selection.alreadyPresentCount,
		AddedItemCount:      len(selection.targetTrackIDs),
		OmittedCount:        selection.omittedCount,
		Truncated:           selection.omittedCount > 0,
	}, nil
}

func normalizedBatchAddItemIDs(values []string) ([]string, bool) {
	if len(values) == 0 || len(values) > MaxPlaylistItems {
		return nil, false
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, false
		}
		if _, exists := seen[value]; exists {
			return nil, false
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, true
}

func lockedPlaylistTracks(
	ctx context.Context,
	tx *sql.Tx,
	playlistID string,
) ([]playlistMergeTrack, error) {
	rows, err := tx.QueryContext(
		ctx,
		`SELECT i.id, i.track_id, t.source, t.external_track_id
		 FROM music_playlist_items i
		 JOIN music_tracks t ON t.id = i.track_id
		 WHERE i.playlist_id = ?
		 ORDER BY i.sort_order ASC, i.created_at ASC, i.id ASC
		 FOR UPDATE`,
		playlistID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tracks := make([]playlistMergeTrack, 0, MaxPlaylistItems)
	for rows.Next() {
		var track playlistMergeTrack
		if err := rows.Scan(
			&track.itemID,
			&track.trackID,
			&track.source,
			&track.externalTrackID,
		); err != nil {
			return nil, err
		}
		tracks = append(tracks, track)
	}
	return tracks, rows.Err()
}

func selectBatchAddTracks(
	selected []playlistMergeTrack,
	existingLinks map[string]struct{},
	available int,
) playlistBatchAddSelection {
	if available < 0 {
		available = 0
	}
	result := playlistBatchAddSelection{
		selectedItemCount: len(selected),
		targetTrackIDs:    make([]string, 0, available),
	}
	selectedLinks := make(map[string]struct{}, len(selected))
	for _, track := range selected {
		linkKey := playlistMergeLinkKey(track)
		if _, duplicate := selectedLinks[linkKey]; duplicate {
			result.duplicateCount++
			continue
		}
		selectedLinks[linkKey] = struct{}{}
		result.uniqueItemCount++
		if _, exists := existingLinks[linkKey]; exists {
			result.alreadyPresentCount++
			continue
		}
		if len(result.targetTrackIDs) >= available {
			result.omittedCount++
			continue
		}
		result.targetTrackIDs = append(result.targetTrackIDs, track.trackID)
		existingLinks[linkKey] = struct{}{}
	}
	return result
}
