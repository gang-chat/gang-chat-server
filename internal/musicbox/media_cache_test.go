package musicbox

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBroadcastCacheKeyIsSharedAcrossRoomsAndEncodingSpecific(t *testing.T) {
	first := broadcastCacheKey("netease", "track-1", "128k")
	second := broadcastCacheKey("netease", "track-1", "128k")
	otherTrack := broadcastCacheKey("netease", "track-2", "128k")
	otherEncoding := broadcastCacheKey("netease", "track-1", "192k")
	if first != second {
		t.Fatalf("stable cache keys differ: %q != %q", first, second)
	}
	if first == otherTrack || first == otherEncoding {
		t.Fatalf("cache key did not separate track/encoding: %q %q %q", first, otherTrack, otherEncoding)
	}

	manager := &Manager{cfg: Config{Dir: t.TempDir(), OpusBitrate: "128k"}}
	left := manager.broadcastCachePath(&QueueItem{RoomID: "room-a", Source: "netease", TrackID: "track-1"})
	right := manager.broadcastCachePath(&QueueItem{RoomID: "room-b", Source: "netease", TrackID: "track-1"})
	if left != right {
		t.Fatalf("same track used room-owned paths: %q != %q", left, right)
	}
}

func TestPrepareBroadcastMediaReusesAnotherRoomsArtifact(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()
	manager := &Manager{
		cfg:   Config{Dir: dir, OpusBitrate: "128k"},
		store: store,
		// tc intentionally remains nil: a miss would panic instead of silently
		// turning this into a transcode test.
	}
	first := &QueueItem{RoomID: "room-a", Source: "netease", TrackID: "track-1", DurationMS: 4321}
	path := manager.broadcastCachePath(first)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	if err := os.WriteFile(path, []byte("shared-opus"), 0o600); err != nil {
		t.Fatalf("write shared artifact: %v", err)
	}
	row, err := store.insertItem(QueueItem{
		ID: "source-row", RoomID: "room-a", Source: "netease",
		TrackID: "track-1", Title: "Shared", Status: StatusPending, SortOrder: 10,
	})
	if err != nil {
		t.Fatalf("insert source row: %v", err)
	}
	if err := store.markReady(row.ID, path, int64(len("shared-opus")), 4321); err != nil {
		t.Fatalf("mark source ready: %v", err)
	}

	result, gotPath, err := manager.prepareBroadcastMedia(
		t.Context(),
		&QueueItem{RoomID: "room-b", Source: "netease", TrackID: "track-1"},
	)
	if err != nil {
		t.Fatalf("reuse cache: %v", err)
	}
	if gotPath != path || result.SizeBytes != int64(len("shared-opus")) || result.DurationMS != 4321 {
		t.Fatalf("cache result = (%q, %+v), want shared artifact metadata", gotPath, result)
	}
	metrics := manager.ObservabilitySnapshot()
	if metrics.CacheHits != 1 || metrics.CacheMisses != 0 {
		t.Fatalf("cache metrics = %+v, want one hit and no miss", metrics)
	}
}

func TestPrefetchWindowLimitsPreparationToCurrentAndNextTwo(t *testing.T) {
	items := []*QueueItem{
		{ID: "one", SortOrder: 10},
		{ID: "two", SortOrder: 20},
		{ID: "three", SortOrder: 30},
		{ID: "four", SortOrder: 40},
	}
	window := prefetchWindow(items, "two", -1, ModeSequential, "room", 3)
	if got := queueItemIDs(window); !equalQueueOrder(got, []string{"two", "three", "four"}) {
		t.Fatalf("sequential prefetch window = %v", got)
	}
	window = prefetchWindow(items, "four", -1, ModeSequential, "room", 3)
	if got := queueItemIDs(window); !equalQueueOrder(got, []string{"four"}) {
		t.Fatalf("sequential tail window = %v", got)
	}
	window = prefetchWindow(items, "four", -1, ModeRepeatAll, "room", 3)
	if got := queueItemIDs(window); !equalQueueOrder(got, []string{"four", "one", "two"}) {
		t.Fatalf("repeat-all wrapped window = %v", got)
	}
}

func TestBroadcastCacheCleanupEvictsLRUAndRepairsQueueRows(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()
	manager := &Manager{
		cfg:          Config{Dir: dir, CacheMaxBytes: 10},
		store:        store,
		players:      map[string]*player{},
		seenCommands: map[string]map[string]int64{},
		playCursors:  map[string]playCursor{},
		cacheLeases:  map[string]int{},
	}
	cacheDir := manager.broadcastCacheDir()
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	oldPath := filepath.Join(cacheDir, "old.ogg")
	keepPath := filepath.Join(cacheDir, "keep.ogg")
	if err := os.WriteFile(oldPath, []byte("12345678"), 0o600); err != nil {
		t.Fatalf("write old cache: %v", err)
	}
	if err := os.WriteFile(keepPath, []byte("abcdefgh"), 0o600); err != nil {
		t.Fatalf("write kept cache: %v", err)
	}
	oldTime := time.Now().Add(-time.Hour)
	_ = os.Chtimes(oldPath, oldTime, oldTime)

	insertReady := func(id, roomID, path string) *QueueItem {
		item, err := store.insertItem(QueueItem{
			ID: id, RoomID: roomID, Source: "netease", TrackID: id,
			Title: id, Status: StatusPending, SortOrder: 10,
		})
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
		if err := store.markReady(item.ID, path, 8, 1000); err != nil {
			t.Fatalf("mark %s ready: %v", id, err)
		}
		return item
	}
	oldItem := insertReady("old-item", "room-old", oldPath)
	keepItem := insertReady("keep-item", "room-keep", keepPath)

	manager.cleanupBroadcastCache(keepPath)
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old cache was not evicted: %v", err)
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Fatalf("kept cache disappeared: %v", err)
	}
	oldAfter, err := store.getItem(oldItem.ID)
	if err != nil {
		t.Fatalf("reload old row: %v", err)
	}
	if oldAfter.Status != StatusPending || oldAfter.FilePath != "" || oldAfter.FileSizeBytes != 0 {
		t.Fatalf("evicted row was not repaired: %+v", oldAfter)
	}
	keepAfter, err := store.getItem(keepItem.ID)
	if err != nil {
		t.Fatalf("reload kept row: %v", err)
	}
	if keepAfter.Status != StatusReady || keepAfter.FilePath != keepPath {
		t.Fatalf("kept row changed: %+v", keepAfter)
	}
	metrics := manager.ObservabilitySnapshot()
	if metrics.CacheEvictions != 1 || metrics.CacheEvictedBytes != 8 {
		t.Fatalf("eviction metrics = %+v, want one 8-byte eviction", metrics)
	}
}

func TestBroadcastCacheCleanupNeverEvictsLeasedFile(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()
	manager := &Manager{
		cfg:          Config{Dir: dir, CacheMaxBytes: 4},
		store:        store,
		players:      map[string]*player{},
		seenCommands: map[string]map[string]int64{},
		playCursors:  map[string]playCursor{},
		cacheLeases:  map[string]int{},
	}
	if err := os.MkdirAll(manager.broadcastCacheDir(), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	leasedPath := filepath.Join(manager.broadcastCacheDir(), "leased.ogg")
	evictablePath := filepath.Join(manager.broadcastCacheDir(), "evictable.ogg")
	for _, path := range []string{leasedPath, evictablePath} {
		if err := os.WriteFile(path, []byte("12345678"), 0o600); err != nil {
			t.Fatalf("write cache %s: %v", path, err)
		}
	}
	release := manager.acquireMediaLease(&QueueItem{FilePath: leasedPath})
	manager.cleanupBroadcastCache("")
	if _, err := os.Stat(leasedPath); err != nil {
		t.Fatalf("leased cache disappeared: %v", err)
	}
	if _, err := os.Stat(evictablePath); !os.IsNotExist(err) {
		t.Fatalf("unleased cache was not evicted: %v", err)
	}
	release()
	deadline := time.Now().Add(time.Second)
	for {
		_, err := os.Stat(leasedPath)
		if os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("released cache was not reconsidered for eviction: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBroadcastCacheCleanupProtectsSelectedFileBeforePlayerOpensIt(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()
	manager := &Manager{
		cfg:          Config{Dir: dir, CacheMaxBytes: 4},
		store:        store,
		players:      map[string]*player{},
		seenCommands: map[string]map[string]int64{},
		playCursors:  map[string]playCursor{},
		cacheLeases:  map[string]int{},
	}
	if err := os.MkdirAll(manager.broadcastCacheDir(), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	selectedPath := filepath.Join(manager.broadcastCacheDir(), "selected.ogg")
	evictablePath := filepath.Join(manager.broadcastCacheDir(), "evictable.ogg")
	for _, path := range []string{selectedPath, evictablePath} {
		if err := os.WriteFile(path, []byte("12345678"), 0o600); err != nil {
			t.Fatalf("write cache %s: %v", path, err)
		}
	}
	selected, err := store.insertItem(QueueItem{
		ID: "selected-item", RoomID: "room-selected", Source: "netease",
		TrackID: "selected", Title: "Selected", Status: StatusPending, SortOrder: 10,
	})
	if err != nil {
		t.Fatalf("insert selected row: %v", err)
	}
	if err := store.markReady(selected.ID, selectedPath, 8, 1000); err != nil {
		t.Fatalf("mark selected ready: %v", err)
	}
	state, err := store.ensureState(selected.RoomID)
	if err != nil {
		t.Fatalf("ensure state: %v", err)
	}
	state.CurrentItemID = selected.ID
	state.State = StatePlaying
	if err := store.saveState(*state); err != nil {
		t.Fatalf("save selected state: %v", err)
	}

	manager.cleanupBroadcastCache("")
	if _, err := os.Stat(selectedPath); err != nil {
		t.Fatalf("selected cache disappeared before player lease: %v", err)
	}
	if _, err := os.Stat(evictablePath); !os.IsNotExist(err) {
		t.Fatalf("unselected cache was not evicted: %v", err)
	}
}

func TestReleaseQueueMediaKeepsSharedCacheAndRemovesUnreferencedLegacyFile(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()
	manager := &Manager{
		cfg:         Config{Dir: dir, CacheMaxBytes: 1 << 20},
		store:       store,
		cacheLeases: map[string]int{},
	}
	if err := os.MkdirAll(manager.broadcastCacheDir(), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	sharedPath := filepath.Join(manager.broadcastCacheDir(), "shared.ogg")
	legacyPath := filepath.Join(dir, "room-legacy", "legacy.ogg")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatalf("mkdir legacy room: %v", err)
	}
	for _, path := range []string{sharedPath, legacyPath} {
		if err := os.WriteFile(path, []byte("opus"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	insertReady := func(id, roomID, path string) *QueueItem {
		item, err := store.insertItem(QueueItem{
			ID: id, RoomID: roomID, Source: "netease", TrackID: id,
			Title: id, Status: StatusPending, SortOrder: 10,
		})
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
		if err := store.markReady(item.ID, path, 4, 1000); err != nil {
			t.Fatalf("mark %s ready: %v", id, err)
		}
		removed, err := store.deleteRoomItem(roomID, item.ID)
		if err != nil {
			t.Fatalf("delete %s: %v", id, err)
		}
		return removed
	}

	manager.releaseQueueMedia([]*QueueItem{
		insertReady("shared-item", "room-shared", sharedPath),
		insertReady("legacy-item", "room-legacy", legacyPath),
	})
	if _, err := os.Stat(sharedPath); err != nil {
		t.Fatalf("shared cache should outlive queue row: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("unreferenced legacy file was not removed: %v", err)
	}
}

func TestPrefetchWindowSkipsFailedRows(t *testing.T) {
	items := []*QueueItem{
		{ID: "f1", SortOrder: 10, Status: StatusFailed},
		{ID: "f2", SortOrder: 20, Status: StatusFailed},
		{ID: "f3", SortOrder: 30, Status: StatusFailed},
		{ID: "p4", SortOrder: 40, Status: StatusPending},
		{ID: "p5", SortOrder: 50, Status: StatusPending},
		{ID: "f6", SortOrder: 60, Status: StatusFailed},
		{ID: "p7", SortOrder: 70, Status: StatusPending},
		{ID: "p8", SortOrder: 80, Status: StatusPending},
	}
	// A playlist whose first songs all failed must still download the rest.
	window := prefetchWindow(items, "", -1, ModeSequential, "room", 3)
	if got := queueItemIDs(window); !equalQueueOrder(got, []string{"p4", "p5", "p7"}) {
		t.Fatalf("window after failed head = %v, want p4/p5/p7", got)
	}
	if next := firstPendingItem(window); next == nil || next.ID != "p4" {
		t.Fatalf("next download = %+v, want p4", next)
	}
	// A failed current row doesn't take a slot either.
	window = prefetchWindow(items, "f6", -1, ModeSequential, "room", 3)
	if got := queueItemIDs(window); !equalQueueOrder(got, []string{"p7", "p8"}) {
		t.Fatalf("window from failed current = %v, want p7/p8", got)
	}
	window = prefetchWindow(items, "p8", -1, ModeRepeatAll, "room", 3)
	if got := queueItemIDs(window); !equalQueueOrder(got, []string{"p8", "p4", "p5"}) {
		t.Fatalf("repeat-all window over failures = %v, want p8/p4/p5", got)
	}
	allFailed := []*QueueItem{items[0], items[1], items[2]}
	if window := prefetchWindow(allFailed, "", -1, ModeRepeatAll, "room", 3); len(window) != 0 {
		t.Fatalf("all-failed window = %v, want empty", queueItemIDs(window))
	}
}

func TestPreparedBytesCountsOnlySongsStillToPlay(t *testing.T) {
	const songBytes = 4 << 20
	const capBytes = 200 << 20
	// A long session: 60 songs already played (240 MB, over the cap) and two
	// still waiting. Played songs must not stop the next download.
	items := make([]*QueueItem, 0, 62)
	for index := 1; index <= 60; index++ {
		items = append(items, &QueueItem{
			ID: fmt.Sprintf("played-%d", index), SortOrder: int64(index * 10),
			Status: StatusReady, FileSizeBytes: songBytes,
		})
	}
	items = append(items,
		&QueueItem{ID: "next-1", SortOrder: 610, Status: StatusPending},
		&QueueItem{ID: "next-2", SortOrder: 620, Status: StatusPending},
	)
	window := prefetchWindow(items, "", 600, ModeSequential, "room", broadcastPrefetchWindow)
	if used := preparedBytes(window); used != 0 {
		t.Fatalf("prepared bytes after the cursor = %d, want 0", used)
	}
	if next := firstPendingItem(window); next == nil || next.ID != "next-1" {
		t.Fatalf("next download = %+v, want next-1", next)
	}

	// While a song plays, its audio and the songs prepared after it count.
	window = prefetchWindow(items, "played-59", -1, ModeSequential, "room", broadcastPrefetchWindow)
	if used := preparedBytes(window); used != 2*songBytes || used >= capBytes {
		t.Fatalf("prepared bytes while playing = %d, want %d", used, 2*songBytes)
	}
	if next := firstPendingItem(window); next == nil || next.ID != "next-1" {
		t.Fatalf("next download while playing = %+v, want next-1", next)
	}
}

func TestRemoveStaleTempFilesKeepsFreshAndFinishedFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name string, age time.Duration) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		modified := now.Add(-age)
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatalf("age %s: %v", name, err)
		}
		return path
	}
	abandoned := write("abc.ogg.tmp-1111", time.Hour)
	abandonedPreview := write("abc.m4a.tmp-2222", time.Hour)
	inFlight := write("def.ogg.tmp-3333", time.Minute)
	finished := write("abc.ogg", time.Hour)

	if removed := removeStaleTempFiles(dir, staleTempFileAge, now); removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	for _, path := range []string{abandoned, abandonedPreview} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("abandoned temp file %s still exists: %v", filepath.Base(path), err)
		}
	}
	for _, path := range []string{inFlight, finished} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was removed: %v", filepath.Base(path), err)
		}
	}
	if removed := removeStaleTempFiles(filepath.Join(dir, "missing"), staleTempFileAge, now); removed != 0 {
		t.Fatalf("missing dir removed = %d, want 0", removed)
	}
}

func TestPlayCursorMatchesOnlyItsQueue(t *testing.T) {
	manager := &Manager{playCursors: map[string]playCursor{}}
	manager.setCursor("r1", QueueScopeSavedPlaylistSnapshot, "snapshot-1", 50)
	if got := manager.cursorAfter("r1", QueueScopeSavedPlaylistSnapshot, "snapshot-1"); got != 50 {
		t.Fatalf("cursor = %d, want 50", got)
	}
	for _, tc := range []struct {
		room     string
		scope    QueueScope
		snapshot string
	}{
		{room: "r1", scope: QueueScopeSavedPlaylistSnapshot, snapshot: "snapshot-2"},
		{room: "r1", scope: QueueScopeTemporary},
		{room: "r2", scope: QueueScopeSavedPlaylistSnapshot, snapshot: "snapshot-1"},
	} {
		if got := manager.cursorAfter(tc.room, tc.scope, tc.snapshot); got != -1 {
			t.Fatalf("cursor for %+v = %d, want -1", tc, got)
		}
	}
}

func TestPlayCursorSurvivesRestartAndFullStateSaves(t *testing.T) {
	store := newTestStore(t)
	state, err := store.ensureState("r1")
	if err != nil {
		t.Fatalf("ensure state: %v", err)
	}
	before := &Manager{store: store, playCursors: map[string]playCursor{}}
	before.setCursor("r1", QueueScopeTemporary, "", 230)
	// Other code saves the whole state row from copies read earlier; that
	// must not revert the cursor.
	state.Revision++
	if err := store.saveState(*state); err != nil {
		t.Fatalf("save state: %v", err)
	}

	restarted := &Manager{store: store, playCursors: map[string]playCursor{}}
	if got := restarted.cursorAfter("r1", QueueScopeTemporary, ""); got != 230 {
		t.Fatalf("cursor after restart = %d, want 230", got)
	}
	if got := restarted.cursorAfter("r1", QueueScopeSavedPlaylistSnapshot, "s1"); got != -1 {
		t.Fatalf("cursor for another queue = %d, want -1", got)
	}
	if got := restarted.cursorAfter("no-such-room", QueueScopeTemporary, ""); got != -1 {
		t.Fatalf("cursor for a room without state = %d, want -1", got)
	}

	// An emptied room's expiry clears its queue and its position with it.
	add(t, store, "queued", 240)
	if _, changed, err := store.expireRoomPlayback("r1"); err != nil || !changed {
		t.Fatalf("expire room = (%v, %v), want changed", changed, err)
	}
	afterExpiry := &Manager{store: store, playCursors: map[string]playCursor{}}
	if got := afterExpiry.cursorAfter("r1", QueueScopeTemporary, ""); got != -1 {
		t.Fatalf("cursor after the room expired = %d, want -1", got)
	}
}
