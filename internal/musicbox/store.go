package musicbox

import (
	"database/sql"
	"time"
)

// store is the DB access layer for the music box. All times are unix millis to
// match the rest of the schema.
type store struct {
	db *sql.DB
}

func nowMillis() int64 { return time.Now().UTC().UnixMilli() }

func ns(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func scanItem(row interface {
	Scan(dest ...any) error
}) (*QueueItem, error) {
	var it QueueItem
	var status string
	var picID, lyricID, filePath, errMsg sql.NullString
	var duration sql.NullInt64
	if err := row.Scan(
		&it.ID, &it.RoomID, &it.Source, &it.TrackID, &it.Title, &it.Artist, &it.Album,
		&picID, &lyricID, &duration, &status, &filePath, &it.FileSizeBytes, &errMsg,
		&it.AddedByUserID, &it.SortOrder, &it.CreatedAt, &it.UpdatedAt,
	); err != nil {
		return nil, err
	}
	it.PicID = picID.String
	it.LyricID = lyricID.String
	it.FilePath = filePath.String
	it.Error = errMsg.String
	it.DurationMS = duration.Int64
	it.Status = QueueStatus(status)
	return &it, nil
}

const itemColumns = `id, room_id, source, track_id, title, artist, album,
	pic_id, lyric_id, duration_ms, status, file_path, file_size_bytes, error,
	added_by_user_id, sort_order, created_at, updated_at`

// insertItem adds a new queue row in pending status and returns it.
func (s *store) insertItem(it QueueItem) (*QueueItem, error) {
	now := nowMillis()
	it.CreatedAt = now
	it.UpdatedAt = now
	if it.Status == "" {
		it.Status = StatusPending
	}
	_, err := s.db.Exec(
		`INSERT INTO room_music_box_queue
		 (id, room_id, source, track_id, title, artist, album, pic_id, lyric_id,
		  duration_ms, status, file_path, file_size_bytes, error,
		  added_by_user_id, sort_order, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, 0, NULL, ?, ?, ?, ?)`,
		it.ID, it.RoomID, it.Source, it.TrackID, it.Title, it.Artist, it.Album,
		ns(it.PicID), ns(it.LyricID), nullableDuration(it.DurationMS), string(it.Status),
		it.AddedByUserID, it.SortOrder, it.CreatedAt, it.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &it, nil
}

func nullableDuration(ms int64) any {
	if ms <= 0 {
		return nil
	}
	return ms
}

func (s *store) getItem(id string) (*QueueItem, error) {
	row := s.db.QueryRow(`SELECT `+itemColumns+` FROM room_music_box_queue WHERE id = ?`, id)
	return scanItem(row)
}

// listQueue returns a room's queue in play order.
func (s *store) listQueue(roomID string) ([]*QueueItem, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumns+` FROM room_music_box_queue
		 WHERE room_id = ? ORDER BY sort_order ASC, created_at ASC`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []*QueueItem
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// roomBytes sums transcoded bytes already counted against a room's cap:
// ready files plus an estimate reserved for in-flight (pending/downloading)
// items. Since pending items have no file yet, only ready bytes are real;
// callers add the projected size of a new item before comparing to the cap.
func (s *store) roomReadyBytes(roomID string) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRow(
		`SELECT COALESCE(SUM(file_size_bytes), 0) FROM room_music_box_queue
		 WHERE room_id = ? AND status IN ('ready', 'downloading')`, roomID).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// countInFlight returns how many items are pending or downloading in a room.
func (s *store) countInFlight(roomID string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM room_music_box_queue
		 WHERE room_id = ? AND status IN ('pending', 'downloading')`, roomID).Scan(&n)
	return n, err
}

func (s *store) nextSortOrder(roomID string) (int64, error) {
	var max sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MAX(sort_order) FROM room_music_box_queue WHERE room_id = ?`, roomID).Scan(&max)
	if err != nil {
		return 0, err
	}
	if !max.Valid {
		return 10, nil
	}
	return max.Int64 + 10, nil
}

func (s *store) setStatus(id string, status QueueStatus) error {
	_, err := s.db.Exec(
		`UPDATE room_music_box_queue SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), nowMillis(), id)
	return err
}

func (s *store) markReady(id, filePath string, sizeBytes, durationMS int64) error {
	_, err := s.db.Exec(
		`UPDATE room_music_box_queue
		 SET status = 'ready', file_path = ?, file_size_bytes = ?,
		     duration_ms = COALESCE(NULLIF(?, 0), duration_ms), error = NULL, updated_at = ?
		 WHERE id = ?`,
		filePath, sizeBytes, durationMS, nowMillis(), id)
	return err
}

func (s *store) markFailed(id, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE room_music_box_queue SET status = 'failed', error = ?, updated_at = ? WHERE id = ?`,
		errMsg, nowMillis(), id)
	return err
}

// deleteItem removes a row and returns it (so the caller can clean up the file
// and adjust the player). Returns nil if the row doesn't exist.
func (s *store) deleteItem(id string) (*QueueItem, error) {
	it, err := s.getItem(id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`DELETE FROM room_music_box_queue WHERE id = ?`, id); err != nil {
		return nil, err
	}
	return it, nil
}

// firstReadyAfter returns the first ready item at or after the given sort
// order boundary, used to choose the next track. If afterSort is negative it
// returns the first ready item in the room.
func (s *store) firstPlayable(roomID string, afterSort int64) (*QueueItem, error) {
	row := s.db.QueryRow(
		`SELECT `+itemColumns+` FROM room_music_box_queue
		 WHERE room_id = ? AND status = 'ready' AND sort_order > ?
		 ORDER BY sort_order ASC, created_at ASC LIMIT 1`, roomID, afterSort)
	it, err := scanItem(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return it, err
}

// State persistence ---------------------------------------------------------

func (s *store) ensureState(roomID string) (*RoomState, error) {
	_, _ = s.db.Exec(
		`INSERT OR IGNORE INTO room_music_box_state (room_id, state, position_ms, volume, updated_at)
		 VALUES (?, 'stopped', 0, 100, ?)`, roomID, nowMillis())
	return s.getState(roomID)
}

func (s *store) getState(roomID string) (*RoomState, error) {
	var st RoomState
	var state string
	var current sql.NullString
	err := s.db.QueryRow(
		`SELECT room_id, state, current_item_id, position_ms, volume, updated_at
		 FROM room_music_box_state WHERE room_id = ?`, roomID).
		Scan(&st.RoomID, &state, &current, &st.PositionMS, &st.Volume, &st.UpdatedAt)
	if err == sql.ErrNoRows {
		return &RoomState{RoomID: roomID, State: StateStopped, Volume: 100}, nil
	}
	if err != nil {
		return nil, err
	}
	st.State = PlaybackState(state)
	st.CurrentItemID = current.String
	return &st, nil
}

func (s *store) saveState(st RoomState) error {
	_, err := s.db.Exec(
		`INSERT INTO room_music_box_state (room_id, state, current_item_id, position_ms, volume, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(room_id) DO UPDATE SET
		   state = excluded.state, current_item_id = excluded.current_item_id,
		   position_ms = excluded.position_ms, volume = excluded.volume,
		   updated_at = excluded.updated_at`,
		st.RoomID, string(st.State), ns(st.CurrentItemID), st.PositionMS, st.Volume, nowMillis())
	return err
}
