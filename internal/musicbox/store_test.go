package musicbox

import (
	"database/sql"
	"path/filepath"
	"testing"

	gangdb "github.com/zhuangkaiyi/gang-chat/server/internal/db"
)

// newTestStore uses a new SQLite database with just the schema the store needs
// (users, rooms, playlists, tracks, and the two music box tables), avoiding a
// dependency on the full production schema.
func newTestStore(t *testing.T) *store {
	t.Helper()
	db, err := sql.Open("sqlite", gangdb.DSN(filepath.Join(t.TempDir(), "musicbox.db")))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(10)
	t.Cleanup(func() { db.Close() })

	stmts := []string{
		`CREATE TABLE users (id TEXT PRIMARY KEY)`,
		`CREATE TABLE rooms (id TEXT PRIMARY KEY)`,
		`CREATE TABLE music_playlists (
			id TEXT PRIMARY KEY NOT NULL,
			created_at INTEGER NOT NULL)`,
		`CREATE TABLE music_tracks (
			id TEXT PRIMARY KEY NOT NULL,
			source TEXT NOT NULL, external_track_id TEXT NOT NULL,
			duration_ms INTEGER NULL, updated_at INTEGER NOT NULL DEFAULT 0,
			UNIQUE (source, external_track_id))`,
		`CREATE TABLE room_music_box_queue (
			id TEXT PRIMARY KEY NOT NULL, room_id TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT 'netease', track_id TEXT NOT NULL,
			title TEXT NOT NULL, artist TEXT NOT NULL,
			duration_ms INTEGER, status TEXT NOT NULL DEFAULT 'pending',
			file_path TEXT, file_size_bytes INTEGER NOT NULL DEFAULT 0, error TEXT,
			added_by_user_id TEXT NOT NULL,
			queue_scope TEXT NOT NULL DEFAULT 'temporary', snapshot_id TEXT,
			sort_order INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE room_music_box_state (
			room_id TEXT PRIMARY KEY NOT NULL, state TEXT NOT NULL DEFAULT 'stopped',
			current_item_id TEXT, position_ms INTEGER NOT NULL DEFAULT 0,
			volume INTEGER NOT NULL DEFAULT 100, revision INTEGER NOT NULL DEFAULT 0,
			playback_mode TEXT NOT NULL DEFAULT 'sequential',
			active_source_type TEXT NOT NULL DEFAULT 'temporary',
			active_playlist_id TEXT, active_playlist_name TEXT,
			active_playlist_owner_id TEXT,
			active_playlist_created_at INTEGER NOT NULL DEFAULT 0,
			active_snapshot_id TEXT,
			play_cursor_scope TEXT, play_cursor_snapshot_id TEXT,
			play_cursor_after_sort INTEGER NOT NULL DEFAULT -1,
			updated_at INTEGER NOT NULL)`,
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
	st.Revision = 7
	st.PlaybackMode = ModeRepeatAll
	st.ActiveSourceType = ActiveSourceRoomPlaylist
	st.ActivePlaylistID = "playlist-1"
	st.ActivePlaylistName = "Shared favorites"
	st.ActivePlaylistCreatedAt = 1775444645000
	st.ActiveSnapshotID = "snapshot-1"
	if err := s.saveState(*st); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	got, err := s.getState("r1")
	if err != nil {
		t.Fatalf("getState: %v", err)
	}
	if got.State != StatePlaying || got.CurrentItemID != "a" || got.PositionMS != 1234 ||
		got.Revision != 7 || got.PlaybackMode != ModeRepeatAll ||
		got.ActiveSourceType != ActiveSourceRoomPlaylist ||
		got.ActivePlaylistID != "playlist-1" ||
		got.ActivePlaylistName != "Shared favorites" ||
		got.ActivePlaylistCreatedAt != 1775444645000 ||
		got.ActiveSnapshotID != "snapshot-1" {
		t.Errorf("round trip = %+v", got)
	}
}

func TestBackfillActivePlaylistCreatedAt(t *testing.T) {
	s := newTestStore(t)
	st, err := s.ensureState("r1")
	if err != nil {
		t.Fatalf("ensureState: %v", err)
	}
	st.ActiveSourceType = ActiveSourceRoomPlaylist
	st.ActivePlaylistID = "legacy-playlist"
	if err := s.saveState(*st); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO music_playlists (id, created_at) VALUES (?, ?)`,
		"legacy-playlist",
		int64(1775444645000),
	); err != nil {
		t.Fatalf("insert playlist: %v", err)
	}
	if err := s.backfillActivePlaylistCreatedAt(); err != nil {
		t.Fatalf("backfillActivePlaylistCreatedAt: %v", err)
	}
	got, err := s.getState("r1")
	if err != nil {
		t.Fatalf("getState: %v", err)
	}
	if got.ActivePlaylistCreatedAt != 1775444645000 {
		t.Fatalf("created at = %d, want 1775444645000", got.ActivePlaylistCreatedAt)
	}
}

func insertCatalogTrack(t *testing.T, s *store, id, source, trackID string, durationMS any) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO music_tracks (id, source, external_track_id, duration_ms)
		 VALUES (?, ?, ?, ?)`,
		id, source, trackID, durationMS,
	); err != nil {
		t.Fatalf("insert track %s: %v", id, err)
	}
}

func catalogTrackDuration(t *testing.T, s *store, id string) int64 {
	t.Helper()
	var duration sql.NullInt64
	if err := s.db.QueryRow(
		`SELECT duration_ms FROM music_tracks WHERE id = ?`, id,
	).Scan(&duration); err != nil {
		t.Fatalf("read track %s: %v", id, err)
	}
	return duration.Int64
}

func TestRecordTrackDurationFillsOnlyUnknownCatalogDurations(t *testing.T) {
	s := newTestStore(t)
	insertCatalogTrack(t, s, "unknown", "netease", "111", nil)
	insertCatalogTrack(t, s, "zero", "netease", "222", 0)
	insertCatalogTrack(t, s, "known", "netease", "333", 180000)
	insertCatalogTrack(t, s, "other-source", "bilibili", "111", nil)

	for _, track := range []struct {
		trackID    string
		durationMS int64
	}{
		{"111", 201000},
		{"222", 95000},
		{"333", 30000},
	} {
		if err := s.recordTrackDuration("netease", track.trackID, track.durationMS); err != nil {
			t.Fatalf("recordTrackDuration %s: %v", track.trackID, err)
		}
	}
	if err := s.recordTrackDuration("bilibili", "111", 0); err != nil {
		t.Fatalf("recordTrackDuration without a measurement: %v", err)
	}

	for id, want := range map[string]int64{
		"unknown":      201000,
		"zero":         95000,
		"known":        180000,
		"other-source": 0,
	} {
		if got := catalogTrackDuration(t, s, id); got != want {
			t.Errorf("%s duration = %d, want %d", id, got, want)
		}
	}
}

func TestBackfillTrackDurationsUsesMeasuredQueueLengths(t *testing.T) {
	s := newTestStore(t)
	insertCatalogTrack(t, s, "measured", "netease", "111", nil)
	insertCatalogTrack(t, s, "known", "netease", "222", 180000)
	insertCatalogTrack(t, s, "pending-only", "netease", "333", nil)

	for _, row := range []struct {
		id, trackID string
		durationMS  int64
		ready       bool
	}{
		{"short", "111", 30000, true},
		{"full", "111", 201000, true},
		{"remeasured", "222", 1000, true},
		{"pending", "333", 99000, false},
	} {
		if _, err := s.insertItem(QueueItem{
			ID: row.id, RoomID: "r1", Source: "netease", TrackID: row.trackID,
			Title: row.id, AddedByUserID: "u1", DurationMS: row.durationMS,
		}); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
		if row.ready {
			if err := s.markReady(row.id, "/tmp/"+row.id+".ogg", 1, row.durationMS); err != nil {
				t.Fatalf("markReady %s: %v", row.id, err)
			}
		}
	}

	if err := s.backfillTrackDurations(); err != nil {
		t.Fatalf("backfillTrackDurations: %v", err)
	}

	for id, want := range map[string]int64{
		"measured":     201000,
		"known":        180000,
		"pending-only": 0,
	} {
		if got := catalogTrackDuration(t, s, id); got != want {
			t.Errorf("%s duration = %d, want %d", id, got, want)
		}
	}
}

func TestScopedQueuesKeepTemporaryAndSavedSnapshotIndependent(t *testing.T) {
	s := newTestStore(t)
	add(t, s, "temporary", 10)
	_, err := s.insertItem(QueueItem{
		ID: "saved", RoomID: "r1", Source: "netease", TrackID: "saved-track",
		Title: "Saved", AddedByUserID: "u1", SortOrder: 10,
		QueueScope: QueueScopeSavedPlaylistSnapshot, SnapshotID: "snapshot-1",
	})
	if err != nil {
		t.Fatalf("insert saved snapshot: %v", err)
	}

	temporary, err := s.listScopedQueue("r1", QueueScopeTemporary, "")
	if err != nil || len(temporary) != 1 || temporary[0].ID != "temporary" {
		t.Fatalf("temporary queue = %+v, err=%v", temporary, err)
	}
	saved, err := s.listScopedQueue(
		"r1",
		QueueScopeSavedPlaylistSnapshot,
		"snapshot-1",
	)
	if err != nil || len(saved) != 1 || saved[0].ID != "saved" {
		t.Fatalf("saved queue = %+v, err=%v", saved, err)
	}
}

func TestDeleteRoomItemDoesNotCrossRoomBoundary(t *testing.T) {
	s := newTestStore(t)
	add(t, s, "a", 10)

	deleted, err := s.deleteRoomItem("another-room", "a")
	if err != nil {
		t.Fatalf("delete wrong room: %v", err)
	}
	if deleted != nil {
		t.Fatalf("wrong-room delete returned %+v", deleted)
	}
	item, err := s.getRoomItem("r1", "a")
	if err != nil || item == nil {
		t.Fatalf("item should still exist: item=%+v err=%v", item, err)
	}
}

func TestFirstPlayableInScopeIncludesOnlyStreamableDownloadingRows(t *testing.T) {
	s := newTestStore(t)
	add(t, s, "pending", 10)
	add(t, s, "cold", 20)
	add(t, s, "streaming", 30)
	add(t, s, "ready", 40)
	add(t, s, "failed", 50)
	for id, status := range map[string]QueueStatus{
		"cold":      StatusDownloading,
		"streaming": StatusDownloading,
		"failed":    StatusFailed,
	} {
		if err := s.setStatus(id, status); err != nil {
			t.Fatalf("set %s status: %v", id, err)
		}
	}
	if err := s.markReady("ready", "/tmp/ready.ogg", 10, 0); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	streaming := func(item *QueueItem) bool { return item.ID == "streaming" }
	everything := func(*QueueItem) bool { return true }

	cases := []struct {
		name   string
		filter streamingFilter
		first  string
		last   string
	}{
		{name: "ready only", filter: nil, first: "ready", last: "ready"},
		{name: "streamable row", filter: streaming, first: "streaming", last: "ready"},
		// The filter is only consulted for downloading rows: pending and
		// failed rows never become playable.
		{name: "permissive filter", filter: everything, first: "cold", last: "ready"},
	}
	for _, tc := range cases {
		first, err := s.firstPlayableInScope("r1", -1, QueueScopeTemporary, "", tc.filter)
		if err != nil {
			t.Fatalf("%s: first playable: %v", tc.name, err)
		}
		if first == nil || first.ID != tc.first {
			t.Fatalf("%s: first playable = %+v, want %s", tc.name, first, tc.first)
		}
		last, err := s.lastPlayableBefore("r1", 60, QueueScopeTemporary, "", tc.filter)
		if err != nil {
			t.Fatalf("%s: last playable: %v", tc.name, err)
		}
		if last == nil || last.ID != tc.last {
			t.Fatalf("%s: last playable = %+v, want %s", tc.name, last, tc.last)
		}
	}
	before, err := s.lastPlayableBefore("r1", 40, QueueScopeTemporary, "", streaming)
	if err != nil {
		t.Fatalf("last playable before ready: %v", err)
	}
	if before == nil || before.ID != "streaming" {
		t.Fatalf("last playable before ready = %+v, want streaming", before)
	}
}

func TestProvisionalDurationFillsUnknownAndIsReplacedWhenReady(t *testing.T) {
	s := newTestStore(t)
	unknown := add(t, s, "unknown", 10)
	known, err := s.insertItem(QueueItem{
		ID: "known", RoomID: "r1", Source: "netease", TrackID: "tknown",
		Title: "Known", AddedByUserID: "u1", SortOrder: 20, DurationMS: 180000,
	})
	if err != nil {
		t.Fatalf("insert known: %v", err)
	}
	for _, item := range []*QueueItem{unknown, known} {
		if err := s.setStatus(item.ID, StatusDownloading); err != nil {
			t.Fatalf("set %s downloading: %v", item.ID, err)
		}
		if err := s.setProvisionalDuration(item.ID, 251904); err != nil {
			t.Fatalf("provisional %s: %v", item.ID, err)
		}
	}
	duration := func(id string) int64 {
		t.Helper()
		item, err := s.getItem(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		return item.DurationMS
	}
	if got := duration("unknown"); got != 251904 {
		t.Fatalf("unknown duration = %d, want provisional 251904", got)
	}
	if got := duration("known"); got != 180000 {
		t.Fatalf("known duration = %d, want 180000 kept", got)
	}
	if err := s.markReady("unknown", "/tmp/unknown.ogg", 10, 249500); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	if got := duration("unknown"); got != 249500 {
		t.Fatalf("ready duration = %d, want measured 249500", got)
	}
}

func TestResetInterruptedDownloadsReturnsRowsToPending(t *testing.T) {
	s := newTestStore(t)
	add(t, s, "interrupted", 10)
	add(t, s, "ready", 20)
	add(t, s, "failed", 30)
	add(t, s, "pending", 40)
	if err := s.setStatus("interrupted", StatusDownloading); err != nil {
		t.Fatalf("set downloading: %v", err)
	}
	if err := s.markReady("ready", "/tmp/ready.ogg", 10, 1000); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	if err := s.markFailed("failed", "upstream 403"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	reset, err := s.resetInterruptedDownloads()
	if err != nil {
		t.Fatalf("reset interrupted downloads: %v", err)
	}
	if reset != 1 {
		t.Fatalf("reset rows = %d, want 1", reset)
	}
	if inflight, err := s.countDownloading("r1"); err != nil || inflight != 0 {
		t.Fatalf("downloads in flight = (%d, %v), want 0", inflight, err)
	}
	want := map[string]QueueStatus{
		"interrupted": StatusPending,
		"ready":       StatusReady,
		"failed":      StatusFailed,
		"pending":     StatusPending,
	}
	for id, status := range want {
		item, err := s.getItem(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if item.Status != status {
			t.Fatalf("%s status = %q, want %q", id, item.Status, status)
		}
	}
}
