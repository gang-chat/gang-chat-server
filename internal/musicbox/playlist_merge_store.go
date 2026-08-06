package musicbox

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type playlistMergeTrack struct {
	itemID          string
	trackID         string
	source          string
	externalTrackID string
}

type playlistMergeSource struct {
	playlistID string
	tracks     []playlistMergeTrack
}

type playlistMergeSelection struct {
	targetTrackIDs          []string
	consumedItemIDs         map[string][]string
	deletedPlaylistIDs      []string
	sourceItemCount         int
	uniqueItemCount         int
	consumedSourceItemCount int
}

// MergeUserPlaylists creates one personal playlist from the selected personal
// playlists. Fully consumed sources are deleted; if the 500-item target limit
// is reached partway through a source, only its consumed prefix is removed.
func (s *PlaylistStore) MergeUserPlaylists(
	ctx context.Context,
	ownerUserID, name string,
	playlistIDs []string,
) (PlaylistMergeResult, error) {
	return s.mergePlaylists(
		ctx,
		userPlaylistScope(ownerUserID),
		ownerUserID,
		name,
		playlistIDs,
		MaxUserPlaylists,
	)
}

// MergeRoomPlaylists applies the same consume-and-preserve rule to room
// playlists. The actor is recorded as the adder of copied target items.
func (s *PlaylistStore) MergeRoomPlaylists(
	ctx context.Context,
	roomID, actorUserID, name string,
	playlistIDs []string,
) (PlaylistMergeResult, error) {
	return s.mergePlaylists(
		ctx,
		roomPlaylistScope(roomID),
		actorUserID,
		name,
		playlistIDs,
		MaxRoomPlaylists,
	)
}

func (s *PlaylistStore) mergePlaylists(
	ctx context.Context,
	scope playlistScope,
	actorUserID, name string,
	playlistIDs []string,
	maxPlaylists int,
) (PlaylistMergeResult, error) {
	playlistIDs, ok := normalizedMergePlaylistIDs(playlistIDs)
	if !ok {
		return PlaylistMergeResult{}, ErrPlaylistSelection
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlaylistMergeResult{}, err
	}
	defer tx.Rollback()

	if scope.kind == "room" {
		if err := lockRoomPlaylistNamespace(ctx, tx, scope.roomID); err != nil {
			return PlaylistMergeResult{}, err
		}
	} else {
		var lockedUserID string
		err := tx.QueryRowContext(
			ctx,
			`SELECT id FROM users WHERE id = ? FOR UPDATE`,
			scope.ownerUserID,
		).Scan(&lockedUserID)
		if errors.Is(err, sql.ErrNoRows) {
			return PlaylistMergeResult{}, ErrPlaylistNotFound
		}
		if err != nil {
			return PlaylistMergeResult{}, err
		}
	}

	sources := make([]playlistMergeSource, 0, len(playlistIDs))
	for _, playlistID := range playlistIDs {
		if _, err := s.playlistSummary(
			ctx,
			tx,
			scope,
			playlistID,
			true,
		); err != nil {
			return PlaylistMergeResult{}, err
		}
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
			return PlaylistMergeResult{}, err
		}
		tracks := make([]playlistMergeTrack, 0, MaxPlaylistItems)
		for rows.Next() {
			var track playlistMergeTrack
			if err := rows.Scan(
				&track.itemID,
				&track.trackID,
				&track.source,
				&track.externalTrackID,
			); err != nil {
				rows.Close()
				return PlaylistMergeResult{}, err
			}
			tracks = append(tracks, track)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return PlaylistMergeResult{}, err
		}
		if err := rows.Close(); err != nil {
			return PlaylistMergeResult{}, err
		}
		sources = append(sources, playlistMergeSource{
			playlistID: playlistID,
			tracks:     tracks,
		})
	}
	selection := selectMergedPlaylistTracks(sources, MaxPlaylistItems)
	deletedPlaylistSet := make(map[string]struct{}, len(selection.deletedPlaylistIDs))
	for _, playlistID := range selection.deletedPlaylistIDs {
		deletedPlaylistSet[playlistID] = struct{}{}
	}

	if conflict, err := mergePlaylistNameConflicts(
		ctx,
		tx,
		scope,
		name,
		deletedPlaylistSet,
	); err != nil {
		return PlaylistMergeResult{}, err
	} else if conflict {
		return PlaylistMergeResult{}, ErrPlaylistName
	}
	where, scopeArgs := scope.summaryWhere("")
	var playlistCount int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM music_playlists WHERE `+where,
		scopeArgs...,
	).Scan(&playlistCount); err != nil {
		return PlaylistMergeResult{}, err
	}
	if playlistCount-len(selection.deletedPlaylistIDs) >= maxPlaylists {
		return PlaylistMergeResult{}, ErrPlaylistLimit
	}

	now := nowMillis()
	for _, source := range sources {
		if _, deleted := deletedPlaylistSet[source.playlistID]; deleted {
			if _, err := tx.ExecContext(
				ctx,
				`DELETE FROM music_playlists WHERE id = ?`,
				source.playlistID,
			); err != nil {
				return PlaylistMergeResult{}, err
			}
			continue
		}
		consumedItemIDs := selection.consumedItemIDs[source.playlistID]
		if len(consumedItemIDs) == 0 {
			continue
		}
		if err := deleteMergedPlaylistItems(
			ctx,
			tx,
			source.playlistID,
			consumedItemIDs,
		); err != nil {
			return PlaylistMergeResult{}, err
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE music_playlists
			 SET revision = revision + 1, updated_at = ?
			 WHERE id = ?`,
			now,
			source.playlistID,
		); err != nil {
			return PlaylistMergeResult{}, err
		}
	}

	var maxOrder sql.NullInt64
	if err := tx.QueryRowContext(
		ctx,
		`SELECT MAX(sort_order) FROM music_playlists WHERE `+where,
		scopeArgs...,
	).Scan(&maxOrder); err != nil {
		return PlaylistMergeResult{}, err
	}
	sortOrder := int64(10)
	if maxOrder.Valid {
		sortOrder = maxOrder.Int64 + 10
	}
	playlist := PlaylistSummary{
		ID:        "mbp_" + randomID(),
		Name:      name,
		Revision:  1,
		ItemCount: len(selection.targetTrackIDs),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if scope.kind == "room" {
		_, err = tx.ExecContext(
			ctx,
			`INSERT INTO music_playlists
			 (id, scope_type, owner_user_id, room_id, name, description,
			  revision, sort_order, created_at, updated_at)
			 VALUES (?, 'room', NULL, ?, ?, '', 1, ?, ?, ?)`,
			playlist.ID,
			scope.roomID,
			playlist.Name,
			sortOrder,
			now,
			now,
		)
	} else {
		_, err = tx.ExecContext(
			ctx,
			`INSERT INTO music_playlists
			 (id, scope_type, owner_user_id, room_id, name, description,
			  revision, sort_order, created_at, updated_at)
			 VALUES (?, 'user', ?, NULL, ?, '', 1, ?, ?, ?)`,
			playlist.ID,
			scope.ownerUserID,
			playlist.Name,
			sortOrder,
			now,
			now,
		)
	}
	if err != nil {
		return PlaylistMergeResult{}, err
	}
	for index, trackID := range selection.targetTrackIDs {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO music_playlist_items
			 (id, playlist_id, track_id, added_by_user_id, sort_order,
			  created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"mbpi_"+randomID(),
			playlist.ID,
			trackID,
			actorUserID,
			int64(index+1)*10,
			now,
			now,
		); err != nil {
			return PlaylistMergeResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return PlaylistMergeResult{}, err
	}
	omittedCount := selection.uniqueItemCount - len(selection.targetTrackIDs)
	return PlaylistMergeResult{
		Playlist:                playlist,
		SourceItemCount:         selection.sourceItemCount,
		UniqueItemCount:         selection.uniqueItemCount,
		DuplicateCount:          selection.sourceItemCount - selection.uniqueItemCount,
		OmittedCount:            omittedCount,
		DeletedPlaylistCount:    len(selection.deletedPlaylistIDs),
		RetainedPlaylistCount:   len(sources) - len(selection.deletedPlaylistIDs),
		ConsumedSourceItemCount: selection.consumedSourceItemCount,
		Truncated:               omittedCount > 0,
	}, nil
}

func normalizedMergePlaylistIDs(values []string) ([]string, bool) {
	if len(values) < 2 || len(values) > MaxUserPlaylists {
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

// selectMergedPlaylistTracks treats source + external track ID as the
// canonical link. It consumes ordered source prefixes until 500 unique links
// are retained. Fully consumed sources can then be deleted; the unconsumed
// suffix and all later sources remain untouched.
func selectMergedPlaylistTracks(
	sources []playlistMergeSource,
	limit int,
) playlistMergeSelection {
	result := playlistMergeSelection{
		targetTrackIDs:  make([]string, 0, limit),
		consumedItemIDs: make(map[string][]string),
	}
	allUniqueLinks := make(map[string]struct{})
	for _, source := range sources {
		for _, track := range source.tracks {
			result.sourceItemCount++
			allUniqueLinks[playlistMergeLinkKey(track)] = struct{}{}
		}
	}
	result.uniqueItemCount = len(allUniqueLinks)

	mergedLinks := make(map[string]struct{}, limit)
	stopped := false
	for _, source := range sources {
		consumed := make([]string, 0, len(source.tracks))
		for _, track := range source.tracks {
			linkKey := playlistMergeLinkKey(track)
			if _, duplicate := mergedLinks[linkKey]; duplicate {
				consumed = append(consumed, track.itemID)
				continue
			}
			if len(result.targetTrackIDs) >= limit {
				stopped = true
				break
			}
			mergedLinks[linkKey] = struct{}{}
			result.targetTrackIDs = append(result.targetTrackIDs, track.trackID)
			consumed = append(consumed, track.itemID)
		}
		if len(consumed) != 0 {
			result.consumedItemIDs[source.playlistID] = consumed
			result.consumedSourceItemCount += len(consumed)
		}
		if len(consumed) == len(source.tracks) {
			result.deletedPlaylistIDs = append(
				result.deletedPlaylistIDs,
				source.playlistID,
			)
		}
		if stopped {
			break
		}
	}
	return result
}

func playlistMergeLinkKey(track playlistMergeTrack) string {
	return track.source + "\x00" + track.externalTrackID
}

func mergePlaylistNameConflicts(
	ctx context.Context,
	tx *sql.Tx,
	scope playlistScope,
	name string,
	deletedPlaylistIDs map[string]struct{},
) (bool, error) {
	where, scopeArgs := scope.summaryWhere("")
	args := append(append([]any{}, scopeArgs...), name)
	rows, err := tx.QueryContext(
		ctx,
		`SELECT id FROM music_playlists
		 WHERE `+where+` AND name = ? FOR UPDATE`,
		args...,
	)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var playlistID string
		if err := rows.Scan(&playlistID); err != nil {
			return false, err
		}
		if _, willDelete := deletedPlaylistIDs[playlistID]; !willDelete {
			return true, nil
		}
	}
	return false, rows.Err()
}

func deleteMergedPlaylistItems(
	ctx context.Context,
	tx *sql.Tx,
	playlistID string,
	itemIDs []string,
) error {
	if len(itemIDs) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(itemIDs)), ",")
	args := make([]any, 0, len(itemIDs)+1)
	args = append(args, playlistID)
	for _, itemID := range itemIDs {
		args = append(args, itemID)
	}
	result, err := tx.ExecContext(
		ctx,
		`DELETE FROM music_playlist_items
		 WHERE playlist_id = ? AND id IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != int64(len(itemIDs)) {
		return ErrPlaylistOrder
	}
	return nil
}
