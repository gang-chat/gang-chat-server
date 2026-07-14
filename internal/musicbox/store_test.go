package musicbox

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// newTestStore uses a MySQL test database with just the schema the store needs
// (users, rooms, and the two music box tables), avoiding a dependency on the
// full production schema.
func newTestStore(t *testing.T) *store {
	t.Helper()
	dsn := os.Getenv("GANG_TEST_MUSICBOX_MYSQL_DSN")
	if dsn == "" {
		t.Skip("GANG_TEST_MUSICBOX_MYSQL_DSN is required for MySQL-backed musicbox tests")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	t.Cleanup(func() { db.Close() })
	if err := requireIsolatedMusicboxTestDatabase(db); err != nil {
		t.Fatal(err)
	}

	cleanup := []string{
		`DROP TABLE IF EXISTS room_music_box_queue`,
		`DROP TABLE IF EXISTS room_music_box_state`,
		`DROP TABLE IF EXISTS rooms`,
		`DROP TABLE IF EXISTS users`,
	}
	for _, s := range cleanup {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	}

	stmts := []string{
		`CREATE TABLE users (id VARCHAR(128) PRIMARY KEY) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE rooms (id VARCHAR(128) PRIMARY KEY) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE room_music_box_queue (
			id VARCHAR(128) PRIMARY KEY NOT NULL, room_id VARCHAR(128) NOT NULL,
			source VARCHAR(64) NOT NULL DEFAULT 'netease', track_id VARCHAR(255) NOT NULL,
			title TEXT NOT NULL, artist TEXT NOT NULL,
			duration_ms BIGINT, status VARCHAR(64) NOT NULL DEFAULT 'pending',
			file_path TEXT, file_size_bytes BIGINT NOT NULL DEFAULT 0, error TEXT,
			added_by_user_id VARCHAR(128) NOT NULL, sort_order BIGINT NOT NULL DEFAULT 0,
			created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE room_music_box_state (
			room_id VARCHAR(128) PRIMARY KEY NOT NULL, state VARCHAR(64) NOT NULL DEFAULT 'stopped',
			current_item_id VARCHAR(128), position_ms BIGINT NOT NULL DEFAULT 0,
			volume INT NOT NULL DEFAULT 100, updated_at BIGINT NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
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

func requireIsolatedMusicboxTestDatabase(db *sql.DB) error {
	var name string
	if err := db.QueryRow(`SELECT DATABASE()`).Scan(&name); err != nil {
		return fmt.Errorf("read musicbox test database name: %w", err)
	}
	if !isSafeMusicboxTestDatabaseName(name) {
		return fmt.Errorf("refusing to modify non-test musicbox database %q", name)
	}
	return nil
}

func isSafeMusicboxTestDatabaseName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name != "" && (strings.HasSuffix(name, "_test") || strings.Contains(name, "_test_"))
}

func TestSafeMusicboxTestDatabaseName(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"gang_chat_musicbox_test": true,
		"musicbox_test_parallel":  true,
		" GANG_TEST ":             true,
		"gang_chat":               false,
		"contest":                 false,
		"test":                    false,
		"":                        false,
	}
	for name, want := range tests {
		name, want := name, want
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := isSafeMusicboxTestDatabaseName(name); got != want {
				t.Fatalf("isSafeMusicboxTestDatabaseName(%q) = %v, want %v", name, got, want)
			}
		})
	}
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

func TestFirstPendingOrdersAndSkipsNonPending(t *testing.T) {
	s := newTestStore(t)
	add(t, s, "a", 10)
	add(t, s, "b", 20)
	add(t, s, "c", 30)
	// a is downloading, so the first *pending* is b.
	if err := s.setStatus("a", StatusDownloading); err != nil {
		t.Fatalf("setStatus: %v", err)
	}
	first, err := s.firstPending("r1")
	if err != nil {
		t.Fatalf("firstPending: %v", err)
	}
	if first == nil || first.ID != "b" {
		t.Fatalf("firstPending = %v, want b", first)
	}
	// With everything ready/downloading, none are pending.
	_ = s.markReady("b", "/tmp/b.ogg", 1, 0)
	_ = s.markReady("c", "/tmp/c.ogg", 1, 0)
	none, err := s.firstPending("r1")
	if err != nil {
		t.Fatalf("firstPending: %v", err)
	}
	if none != nil {
		t.Fatalf("expected no pending, got %v", none)
	}
}

func TestCountDownloading(t *testing.T) {
	s := newTestStore(t)
	add(t, s, "a", 10)
	add(t, s, "b", 20)
	if n, _ := s.countDownloading("r1"); n != 0 {
		t.Fatalf("countDownloading = %d, want 0", n)
	}
	_ = s.setStatus("a", StatusDownloading)
	if n, _ := s.countDownloading("r1"); n != 1 {
		t.Fatalf("countDownloading = %d, want 1", n)
	}
	// ready no longer counts as downloading.
	_ = s.markReady("a", "/tmp/a.ogg", 1, 0)
	if n, _ := s.countDownloading("r1"); n != 0 {
		t.Fatalf("countDownloading after ready = %d, want 0", n)
	}
}

func TestClearAllQueues(t *testing.T) {
	s := newTestStore(t)
	add(t, s, "a", 10)
	add(t, s, "b", 20)
	_ = s.setStatus("a", StatusDownloading)
	_ = s.markReady("b", "/tmp/b.ogg", 1, 0)
	_ = s.saveState(RoomState{RoomID: "r1", State: StatePlaying, CurrentItemID: "b", PositionMS: 5000, Volume: 80})

	if err := s.clearAllQueues(); err != nil {
		t.Fatalf("clearAllQueues: %v", err)
	}

	// Every queued track is gone.
	items, _ := s.listQueue("r1")
	if len(items) != 0 {
		t.Fatalf("queue len = %d, want 0", len(items))
	}
	// Playback state is reset to stopped with no current track.
	st, _ := s.getState("r1")
	if st.State != StateStopped {
		t.Fatalf("state = %q, want stopped", st.State)
	}
	if st.CurrentItemID != "" {
		t.Fatalf("current item = %q, want empty", st.CurrentItemID)
	}
	if st.PositionMS != 0 {
		t.Fatalf("position = %d, want 0", st.PositionMS)
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
