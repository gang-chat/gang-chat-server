package musicbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

const (
	MaxUserPlaylists    = 50
	MaxPlaylistItems    = 500
	DefaultPlaylistPage = 50
	MaxPlaylistPage     = 50
)

var (
	ErrPlaylistNotFound  = errors.New("music playlist not found")
	ErrPlaylistLimit     = errors.New("music playlist limit reached")
	ErrPlaylistItemLimit = errors.New("music playlist item limit reached")
	ErrPlaylistOrder     = errors.New("invalid music playlist item order")
)

// PlaylistStore persists reusable playlists independently from the transient
// room player queue. Audio files never belong to a playlist; only normalized
// track metadata is stored here.
type PlaylistStore struct {
	db *sql.DB
}

type PlaylistSummary struct {
	ID          string
	Name        string
	Description string
	Revision    int64
	ItemCount   int
	CreatedAt   int64
	UpdatedAt   int64
}

type PlaylistPage struct {
	Items    []PlaylistSummary
	Page     int
	PageSize int
	Total    int
	HasMore  bool
}

type PlaylistItem struct {
	ID              string
	PlaylistID      string
	ExternalTrackID string
	Source          string
	Title           string
	Artists         []string
	DurationMS      int64
	SortOrder       int64
	CreatedAt       int64
}

type PlaylistItemsPage struct {
	Playlist PlaylistSummary
	Items    []PlaylistItem
	Page     int
	PageSize int
	Total    int
	HasMore  bool
}

type AddPlaylistItemParams struct {
	OwnerUserID     string
	PlaylistID      string
	Source          string
	ExternalTrackID string
	Title           string
	Artists         []string
	DurationMS      int64
}

func NewPlaylistStore(db *sql.DB) *PlaylistStore {
	return &PlaylistStore{db: db}
}

func (s *PlaylistStore) EnsureSchema() error {
	if s == nil || s.db == nil {
		return errors.New("music playlist database is unavailable")
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS music_tracks (
			id VARCHAR(128) NOT NULL,
			source VARCHAR(32) NOT NULL,
			external_track_id VARCHAR(256) NOT NULL,
			title VARCHAR(512) NOT NULL,
			artist VARCHAR(512) NOT NULL DEFAULT '',
			artists_json JSON NOT NULL,
			duration_ms BIGINT NULL,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL,
			PRIMARY KEY (id),
			UNIQUE KEY uq_music_tracks_source_external (source, external_track_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS music_playlists (
			id VARCHAR(128) NOT NULL,
			scope_type VARCHAR(16) NOT NULL,
			owner_user_id VARCHAR(128) NULL,
			room_id VARCHAR(128) NULL,
			name VARCHAR(64) NOT NULL,
			description VARCHAR(512) NOT NULL DEFAULT '',
			revision BIGINT NOT NULL DEFAULT 1,
			sort_order BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL,
			PRIMARY KEY (id),
			UNIQUE KEY uq_music_playlists_user_name (owner_user_id, name),
			KEY idx_music_playlists_user_order (owner_user_id, sort_order, created_at),
			KEY idx_music_playlists_room_order (room_id, sort_order, created_at),
			CONSTRAINT fk_music_playlists_owner FOREIGN KEY (owner_user_id)
				REFERENCES users (id) ON DELETE CASCADE,
			CONSTRAINT fk_music_playlists_room FOREIGN KEY (room_id)
				REFERENCES rooms (id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS music_playlist_items (
			id VARCHAR(128) NOT NULL,
			playlist_id VARCHAR(128) NOT NULL,
			track_id VARCHAR(128) NOT NULL,
			added_by_user_id VARCHAR(128) NOT NULL,
			sort_order BIGINT NOT NULL,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL,
			PRIMARY KEY (id),
			KEY idx_music_playlist_items_order (playlist_id, sort_order, created_at, id),
			KEY idx_music_playlist_items_track (track_id),
			KEY idx_music_playlist_items_adder (added_by_user_id),
			CONSTRAINT fk_music_playlist_items_playlist FOREIGN KEY (playlist_id)
				REFERENCES music_playlists (id) ON DELETE CASCADE,
			CONSTRAINT fk_music_playlist_items_track FOREIGN KEY (track_id)
				REFERENCES music_tracks (id) ON DELETE RESTRICT,
			CONSTRAINT fk_music_playlist_items_adder FOREIGN KEY (added_by_user_id)
				REFERENCES users (id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func normalizePage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = DefaultPlaylistPage
	}
	if pageSize > MaxPlaylistPage {
		pageSize = MaxPlaylistPage
	}
	return page, pageSize
}

func (s *PlaylistStore) ListUserPlaylists(
	ctx context.Context,
	ownerUserID string,
	page, pageSize int,
) (PlaylistPage, error) {
	page, pageSize = normalizePage(page, pageSize)
	var total int
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM music_playlists
		 WHERE scope_type = 'user' AND owner_user_id = ?`,
		ownerUserID,
	).Scan(&total); err != nil {
		return PlaylistPage{}, err
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT p.id, p.name, p.description, p.revision, COUNT(i.id),
		        p.created_at, p.updated_at
		 FROM music_playlists p
		 LEFT JOIN music_playlist_items i ON i.playlist_id = p.id
		 WHERE p.scope_type = 'user' AND p.owner_user_id = ?
		 GROUP BY p.id, p.name, p.description, p.revision, p.sort_order,
		          p.created_at, p.updated_at
		 ORDER BY p.sort_order ASC, p.created_at ASC, p.id ASC
		 LIMIT ? OFFSET ?`,
		ownerUserID,
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

func (s *PlaylistStore) CreateUserPlaylist(
	ctx context.Context,
	ownerUserID, name string,
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
	var count int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM music_playlists
		 WHERE scope_type = 'user' AND owner_user_id = ?`,
		ownerUserID,
	).Scan(&count); err != nil {
		return PlaylistSummary{}, err
	}
	if count >= MaxUserPlaylists {
		return PlaylistSummary{}, ErrPlaylistLimit
	}
	var nextOrder sql.NullInt64
	if err := tx.QueryRowContext(
		ctx,
		`SELECT MAX(sort_order) FROM music_playlists
		 WHERE scope_type = 'user' AND owner_user_id = ?`,
		ownerUserID,
	).Scan(&nextOrder); err != nil {
		return PlaylistSummary{}, err
	}
	sortOrder := int64(10)
	if nextOrder.Valid {
		sortOrder = nextOrder.Int64 + 10
	}
	now := nowMillis()
	item := PlaylistSummary{
		ID:        "mbp_" + randomID(),
		Name:      name,
		Revision:  1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO music_playlists
		 (id, scope_type, owner_user_id, room_id, name, description,
		  revision, sort_order, created_at, updated_at)
		 VALUES (?, 'user', ?, NULL, ?, '', 1, ?, ?, ?)`,
		item.ID,
		ownerUserID,
		item.Name,
		sortOrder,
		now,
		now,
	); err != nil {
		return PlaylistSummary{}, err
	}
	if err := tx.Commit(); err != nil {
		return PlaylistSummary{}, err
	}
	return item, nil
}

func (s *PlaylistStore) DeleteUserPlaylist(
	ctx context.Context,
	ownerUserID, playlistID string,
) (bool, error) {
	result, err := s.db.ExecContext(
		ctx,
		`DELETE FROM music_playlists
		 WHERE id = ? AND scope_type = 'user' AND owner_user_id = ?`,
		playlistID,
		ownerUserID,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

func (s *PlaylistStore) UserPlaylistItems(
	ctx context.Context,
	ownerUserID, playlistID, keyword, source string,
	page, pageSize int,
) (PlaylistItemsPage, error) {
	page, pageSize = normalizePage(page, pageSize)
	playlist, err := s.userPlaylistSummary(ctx, s.db, ownerUserID, playlistID, false)
	if err != nil {
		return PlaylistItemsPage{}, err
	}
	where, args := playlistItemFilter(playlistID, keyword, source)
	var total int
	if err := s.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*)
		 FROM music_playlist_items i
		 JOIN music_tracks t ON t.id = i.track_id
		 WHERE `+where,
		args...,
	).Scan(&total); err != nil {
		return PlaylistItemsPage{}, err
	}
	queryArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT i.id, i.playlist_id, t.external_track_id, t.source, t.title,
		        t.artists_json, COALESCE(t.duration_ms, 0), i.sort_order, i.created_at
		 FROM music_playlist_items i
		 JOIN music_tracks t ON t.id = i.track_id
		 WHERE `+where+`
		 ORDER BY i.sort_order ASC, i.created_at ASC, i.id ASC
		 LIMIT ? OFFSET ?`,
		queryArgs...,
	)
	if err != nil {
		return PlaylistItemsPage{}, err
	}
	defer rows.Close()
	items := make([]PlaylistItem, 0, pageSize)
	for rows.Next() {
		var item PlaylistItem
		var artistsJSON []byte
		if err := rows.Scan(
			&item.ID,
			&item.PlaylistID,
			&item.ExternalTrackID,
			&item.Source,
			&item.Title,
			&artistsJSON,
			&item.DurationMS,
			&item.SortOrder,
			&item.CreatedAt,
		); err != nil {
			return PlaylistItemsPage{}, err
		}
		_ = json.Unmarshal(artistsJSON, &item.Artists)
		if item.Artists == nil {
			item.Artists = []string{}
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return PlaylistItemsPage{}, err
	}
	return PlaylistItemsPage{
		Playlist: playlist,
		Items:    items,
		Page:     page,
		PageSize: pageSize,
		Total:    total,
		HasMore:  page*pageSize < total,
	}, nil
}

func playlistItemFilter(playlistID, keyword, source string) (string, []any) {
	where := "i.playlist_id = ?"
	args := []any{playlistID}
	if keyword = strings.TrimSpace(keyword); keyword != "" {
		where += " AND (t.title LIKE ? OR t.artist LIKE ?)"
		pattern := "%" + keyword + "%"
		args = append(args, pattern, pattern)
	}
	if source = strings.TrimSpace(source); source != "" {
		where += " AND t.source = ?"
		args = append(args, source)
	}
	return where, args
}

func (s *PlaylistStore) AddUserPlaylistItem(
	ctx context.Context,
	params AddPlaylistItemParams,
) (PlaylistItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlaylistItem{}, err
	}
	defer tx.Rollback()
	if _, err := s.userPlaylistSummary(
		ctx,
		tx,
		params.OwnerUserID,
		params.PlaylistID,
		true,
	); err != nil {
		return PlaylistItem{}, err
	}
	var count int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM music_playlist_items WHERE playlist_id = ?`,
		params.PlaylistID,
	).Scan(&count); err != nil {
		return PlaylistItem{}, err
	}
	if count >= MaxPlaylistItems {
		return PlaylistItem{}, ErrPlaylistItemLimit
	}

	artistsJSON, err := json.Marshal(params.Artists)
	if err != nil {
		return PlaylistItem{}, err
	}
	trackID := "mbt_" + randomID()
	now := nowMillis()
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO music_tracks
		 (id, source, external_track_id, title, artist, artists_json,
		  duration_ms, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, 0), ?, ?)
		 ON DUPLICATE KEY UPDATE
		   title = VALUES(title), artist = VALUES(artist),
		   artists_json = VALUES(artists_json),
		   duration_ms = COALESCE(VALUES(duration_ms), duration_ms),
		   updated_at = VALUES(updated_at)`,
		trackID,
		params.Source,
		params.ExternalTrackID,
		params.Title,
		strings.Join(params.Artists, "、"),
		artistsJSON,
		params.DurationMS,
		now,
		now,
	); err != nil {
		return PlaylistItem{}, err
	}
	if err := tx.QueryRowContext(
		ctx,
		`SELECT id FROM music_tracks WHERE source = ? AND external_track_id = ?`,
		params.Source,
		params.ExternalTrackID,
	).Scan(&trackID); err != nil {
		return PlaylistItem{}, err
	}
	var maxOrder sql.NullInt64
	if err := tx.QueryRowContext(
		ctx,
		`SELECT MAX(sort_order) FROM music_playlist_items WHERE playlist_id = ?`,
		params.PlaylistID,
	).Scan(&maxOrder); err != nil {
		return PlaylistItem{}, err
	}
	sortOrder := int64(10)
	if maxOrder.Valid {
		sortOrder = maxOrder.Int64 + 10
	}
	item := PlaylistItem{
		ID:              "mbpi_" + randomID(),
		PlaylistID:      params.PlaylistID,
		ExternalTrackID: params.ExternalTrackID,
		Source:          params.Source,
		Title:           params.Title,
		Artists:         append([]string{}, params.Artists...),
		DurationMS:      params.DurationMS,
		SortOrder:       sortOrder,
		CreatedAt:       now,
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO music_playlist_items
		 (id, playlist_id, track_id, added_by_user_id, sort_order, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		item.ID,
		params.PlaylistID,
		trackID,
		params.OwnerUserID,
		sortOrder,
		now,
		now,
	); err != nil {
		return PlaylistItem{}, err
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE music_playlists
		 SET revision = revision + 1, updated_at = ? WHERE id = ?`,
		now,
		params.PlaylistID,
	); err != nil {
		return PlaylistItem{}, err
	}
	if err := tx.Commit(); err != nil {
		return PlaylistItem{}, err
	}
	return item, nil
}

func (s *PlaylistStore) DeleteUserPlaylistItems(
	ctx context.Context,
	ownerUserID, playlistID string,
	itemIDs []string,
) (int64, error) {
	if len(itemIDs) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := s.userPlaylistSummary(
		ctx,
		tx,
		ownerUserID,
		playlistID,
		true,
	); err != nil {
		return 0, err
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
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if affected > 0 {
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE music_playlists
			 SET revision = revision + 1, updated_at = ? WHERE id = ?`,
			nowMillis(),
			playlistID,
		); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return affected, nil
}

func (s *PlaylistStore) ReorderUserPlaylistItems(
	ctx context.Context,
	ownerUserID, playlistID string,
	itemIDs []string,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := s.userPlaylistSummary(
		ctx,
		tx,
		ownerUserID,
		playlistID,
		true,
	); err != nil {
		return err
	}
	rows, err := tx.QueryContext(
		ctx,
		`SELECT id FROM music_playlist_items
		 WHERE playlist_id = ? ORDER BY sort_order ASC, created_at ASC, id ASC
		 FOR UPDATE`,
		playlistID,
	)
	if err != nil {
		return err
	}
	existing := make([]string, 0, len(itemIDs))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		existing = append(existing, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !sameStringSet(existing, itemIDs) {
		return ErrPlaylistOrder
	}
	now := nowMillis()
	for index, itemID := range itemIDs {
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE music_playlist_items
			 SET sort_order = ?, updated_at = ?
			 WHERE playlist_id = ? AND id = ?`,
			(index+1)*10,
			now,
			playlistID,
			itemID,
		); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE music_playlists
		 SET revision = revision + 1, updated_at = ? WHERE id = ?`,
		now,
		playlistID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PlaylistStore) MoveUserPlaylistItem(
	ctx context.Context,
	ownerUserID, playlistID, itemID string,
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
	if _, err := s.userPlaylistSummary(
		ctx,
		tx,
		ownerUserID,
		playlistID,
		true,
	); err != nil {
		return err
	}
	rows, err := tx.QueryContext(
		ctx,
		`SELECT id FROM music_playlist_items
		 WHERE playlist_id = ? ORDER BY sort_order ASC, created_at ASC, id ASC
		 FOR UPDATE`,
		playlistID,
	)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	index := -1
	for candidateIndex, id := range ids {
		if id == itemID {
			index = candidateIndex
			break
		}
	}
	target := index + direction
	if index < 0 {
		return ErrPlaylistNotFound
	}
	if target < 0 || target >= len(ids) {
		return ErrPlaylistOrder
	}
	ids[index], ids[target] = ids[target], ids[index]
	now := nowMillis()
	for orderedIndex, id := range ids {
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE music_playlist_items SET sort_order = ?, updated_at = ?
			 WHERE playlist_id = ? AND id = ?`,
			(orderedIndex+1)*10,
			now,
			playlistID,
			id,
		); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE music_playlists
		 SET revision = revision + 1, updated_at = ? WHERE id = ?`,
		now,
		playlistID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

type playlistQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *PlaylistStore) userPlaylistSummary(
	ctx context.Context,
	db playlistQuerier,
	ownerUserID, playlistID string,
	forUpdate bool,
) (PlaylistSummary, error) {
	query := `SELECT p.id, p.name, p.description, p.revision,
	                 (SELECT COUNT(*) FROM music_playlist_items i WHERE i.playlist_id = p.id),
	                 p.created_at, p.updated_at
	          FROM music_playlists p
	          WHERE p.id = ? AND p.scope_type = 'user' AND p.owner_user_id = ?`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var item PlaylistSummary
	err := db.QueryRowContext(ctx, query, playlistID, ownerUserID).Scan(
		&item.ID,
		&item.Name,
		&item.Description,
		&item.Revision,
		&item.ItemCount,
		&item.CreatedAt,
		&item.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PlaylistSummary{}, ErrPlaylistNotFound
	}
	return item, err
}

func sameStringSet(existing, requested []string) bool {
	if len(existing) != len(requested) {
		return false
	}
	counts := make(map[string]int, len(existing))
	for _, value := range existing {
		counts[value]++
	}
	for _, value := range requested {
		counts[value]--
		if counts[value] < 0 {
			return false
		}
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}
