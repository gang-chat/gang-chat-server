package musicbox

import (
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
