package musicbox

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// newTestStore spins up an in-file sqlite DB with just the schema the store
// needs (users, rooms, and the two music box tables), avoiding a dependency on
// the full migration set.
func newTestStore(t *testing.T) *store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "mbx-test.db")
	db, err := sql.Open("sqlite3", dsn+"?_foreign_keys=1")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	stmts := []string{
		`CREATE TABLE users (id TEXT PRIMARY KEY)`,
		`CREATE TABLE rooms (id TEXT PRIMARY KEY)`,
		`CREATE TABLE room_music_box_queue (
			id TEXT PRIMARY KEY NOT NULL, room_id TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT 'netease', track_id TEXT NOT NULL,
			title TEXT NOT NULL, artist TEXT NOT NULL DEFAULT '',
			album TEXT NOT NULL DEFAULT '', pic_id TEXT, lyric_id TEXT,
			duration_ms INTEGER, status TEXT NOT NULL DEFAULT 'pending',
			file_path TEXT, file_size_bytes INTEGER NOT NULL DEFAULT 0, error TEXT,
			added_by_user_id TEXT NOT NULL, sort_order INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE room_music_box_state (
			room_id TEXT PRIMARY KEY NOT NULL, state TEXT NOT NULL DEFAULT 'stopped',
			current_item_id TEXT, position_ms INTEGER NOT NULL DEFAULT 0,
			volume INTEGER NOT NULL DEFAULT 100, updated_at INTEGER NOT NULL)`,
		`INSERT INTO users (id) VALUES ('u1')`,
		`INSERT INTO rooms (id) VALUES ('r1')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return &store{db: db}
}

func add(t *testing.T, s *store, id string, sort int64) *QueueItem {
	t.Helper()
	it, err := s.insertItem(QueueItem{
		ID: id, RoomID: "r1", Source: "netease", TrackID: "t" + id,
		Title: "Title " + id, AddedByUserID: "u1", SortOrder: sort,
	})
	if err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
	return it
}

func TestRoomReadyBytesCountsReadyAndDownloading(t *testing.T) {
	s := newTestStore(t)
	add(t, s, "a", 10) // pending, not counted
	add(t, s, "b", 20)
	add(t, s, "c", 30)

	if err := s.markReady("b", "/tmp/b.ogg", 1000, 0); err != nil {
		t.Fatalf("markReady: %v", err)
	}
	if err := s.setStatus("c", StatusDownloading); err != nil {
		t.Fatalf("setStatus: %v", err)
	}
	// downloading items have 0 bytes until ready, so only b's 1000 counts.
	got, err := s.roomReadyBytes("r1")
	if err != nil {
		t.Fatalf("roomReadyBytes: %v", err)
	}
	if got != 1000 {
		t.Errorf("roomReadyBytes = %d, want 1000", got)
	}
}

func TestFirstPlayableSkipsNonReadyAndOrders(t *testing.T) {
	s := newTestStore(t)
	add(t, s, "a", 10)
	add(t, s, "b", 20)
	add(t, s, "c", 30)
	// Only b and c are ready; a is still pending.
	_ = s.markReady("b", "/tmp/b.ogg", 10, 0)
	_ = s.markReady("c", "/tmp/c.ogg", 10, 0)

	first, err := s.firstPlayable("r1", -1)
	if err != nil {
		t.Fatalf("firstPlayable: %v", err)
	}
	if first == nil || first.ID != "b" {
		t.Fatalf("first playable = %v, want b", first)
	}
	// Advancing past b's sort order yields c.
	next, err := s.firstPlayable("r1", first.SortOrder)
	if err != nil {
		t.Fatalf("firstPlayable next: %v", err)
	}
	if next == nil || next.ID != "c" {
		t.Fatalf("next playable = %v, want c", next)
	}
	// Advancing past c yields nothing.
	last, err := s.firstPlayable("r1", next.SortOrder)
	if err != nil {
		t.Fatalf("firstPlayable last: %v", err)
	}
	if last != nil {
		t.Fatalf("expected no item after c, got %v", last)
	}
}

func TestNextSortOrderIncrements(t *testing.T) {
	s := newTestStore(t)
	first, err := s.nextSortOrder("r1")
	if err != nil {
		t.Fatalf("nextSortOrder: %v", err)
	}
	if first != 10 {
		t.Errorf("first sort order = %d, want 10", first)
	}
	add(t, s, "a", first)
	second, err := s.nextSortOrder("r1")
	if err != nil {
		t.Fatalf("nextSortOrder: %v", err)
	}
	if second != 20 {
		t.Errorf("second sort order = %d, want 20", second)
	}
}

func TestDeleteItemReturnsRow(t *testing.T) {
	s := newTestStore(t)
	add(t, s, "a", 10)
	_ = s.markReady("a", "/tmp/a.ogg", 50, 0)

	deleted, err := s.deleteItem("a")
	if err != nil {
		t.Fatalf("deleteItem: %v", err)
	}
	if deleted == nil || deleted.FilePath != "/tmp/a.ogg" {
		t.Fatalf("deleted = %v, want row with file path", deleted)
	}
	// Second delete is a no-op returning nil.
	again, err := s.deleteItem("a")
	if err != nil {
		t.Fatalf("deleteItem again: %v", err)
	}
	if again != nil {
		t.Fatalf("expected nil on missing row, got %v", again)
	}
}

func TestStateRoundTrip(t *testing.T) {
	s := newTestStore(t)
	st, err := s.ensureState("r1")
	if err != nil {
		t.Fatalf("ensureState: %v", err)
	}
	if st.State != StateStopped || st.Volume != 100 {
		t.Fatalf("default state = %+v, want stopped/100", st)
	}
	add(t, s, "a", 10)
	st.State = StatePlaying
	st.CurrentItemID = "a"
	st.PositionMS = 1234
	if err := s.saveState(*st); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	got, err := s.getState("r1")
	if err != nil {
		t.Fatalf("getState: %v", err)
	}
	if got.State != StatePlaying || got.CurrentItemID != "a" || got.PositionMS != 1234 {
		t.Errorf("round trip = %+v", got)
	}
}
