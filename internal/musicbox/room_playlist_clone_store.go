package musicbox

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// CloneRoomPlaylistToUser atomically copies one room playlist into the user's
// personal library. The room display name follows the caller's membership
// remark and falls back to the room name.
func (s *PlaylistStore) CloneRoomPlaylistToUser(
	ctx context.Context,
	ownerUserID, roomID, playlistID string,
) (PlaylistSummary, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlaylistSummary{}, err
	}
	defer tx.Rollback()

	var lockedUserID string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT id FROM users WHERE id = ? FOR UPDATE`,
		ownerUserID,
	).Scan(&lockedUserID); err != nil {
		return PlaylistSummary{}, err
	}

	var roomDisplayName string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COALESCE(NULLIF(TRIM(rm.remark_name), ''), r.name)
		 FROM rooms r
		 LEFT JOIN room_memberships rm
		   ON rm.room_id = r.id AND rm.user_id = ?
		 WHERE r.id = ?
		 FOR UPDATE`,
		ownerUserID,
		roomID,
	).Scan(&roomDisplayName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PlaylistSummary{}, ErrPlaylistNotFound
		}
		return PlaylistSummary{}, err
	}
	roomDisplayName = strings.TrimSpace(roomDisplayName)

	var existingCount int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM music_playlists
		 WHERE scope_type = 'user' AND owner_user_id = ?`,
		ownerUserID,
	).Scan(&existingCount); err != nil {
		return PlaylistSummary{}, err
	}
	if existingCount >= MaxUserPlaylists {
		return PlaylistSummary{}, ErrPlaylistLimit
	}

	source, err := s.playlistSummary(
		ctx,
		tx,
		roomPlaylistScope(roomID),
		playlistID,
		false,
	)
	if err != nil {
		return PlaylistSummary{}, err
	}
	trackIDs, err := playlistTrackIDs(ctx, tx, source.ID)
	if err != nil {
		return PlaylistSummary{}, err
	}
	if len(trackIDs) > MaxPlaylistItems {
		return PlaylistSummary{}, ErrPlaylistItemLimit
	}

	name, err := availableUserPlaylistName(
		ctx,
		tx,
		ownerUserID,
		roomDisplayName+" · "+source.Name,
	)
	if err != nil {
		return PlaylistSummary{}, err
	}
	var maxOrder sql.NullInt64
	if err := tx.QueryRowContext(
		ctx,
		`SELECT MAX(sort_order) FROM music_playlists
		 WHERE scope_type = 'user' AND owner_user_id = ?`,
		ownerUserID,
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
		ItemCount: len(trackIDs),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO music_playlists
		 (id, scope_type, owner_user_id, room_id, name, description,
		  revision, sort_order, created_at, updated_at)
		 VALUES (?, 'user', ?, NULL, ?, '', 1, ?, ?, ?)`,
		playlist.ID,
		ownerUserID,
		playlist.Name,
		sortOrder,
		now,
		now,
	); err != nil {
		return PlaylistSummary{}, err
	}
	for index, trackID := range trackIDs {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO music_playlist_items
			 (id, playlist_id, track_id, added_by_user_id, sort_order,
			  created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"mbpi_"+randomID(),
			playlist.ID,
			trackID,
			ownerUserID,
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

func playlistTrackIDs(
	ctx context.Context,
	tx *sql.Tx,
	playlistID string,
) ([]string, error) {
	rows, err := tx.QueryContext(
		ctx,
		`SELECT track_id FROM music_playlist_items
		 WHERE playlist_id = ?
		 ORDER BY sort_order ASC, created_at ASC, id ASC`,
		playlistID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	trackIDs := make([]string, 0)
	for rows.Next() {
		var trackID string
		if err := rows.Scan(&trackID); err != nil {
			return nil, err
		}
		trackIDs = append(trackIDs, trackID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return trackIDs, nil
}
