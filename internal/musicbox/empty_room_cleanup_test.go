package musicbox

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestEmptyRoomCleanupIsCancelledByReentryAndExpiresTransientState(t *testing.T) {
	store := newTestStore(t)
	mediaPath := filepath.Join(t.TempDir(), "temporary.ogg")
	if err := os.WriteFile(mediaPath, []byte("cached audio"), 0o600); err != nil {
		t.Fatalf("write cached audio: %v", err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO music_playlists (id, created_at) VALUES (?, ?)`,
		"saved-playlist", nowMillis(),
	); err != nil {
		t.Fatalf("insert saved playlist: %v", err)
	}
	if _, err := store.insertItem(QueueItem{
		ID:            "temporary-item",
		RoomID:        "room-1",
		Source:        "netease",
		TrackID:       "temporary-track",
		Title:         "Temporary",
		Status:        StatusReady,
		FilePath:      mediaPath,
		FileSizeBytes: 12,
		AddedByUserID: "user-1",
		QueueScope:    QueueScopeTemporary,
		SortOrder:     10,
	}); err != nil {
		t.Fatalf("insert temporary item: %v", err)
	}
	if err := store.markReady("temporary-item", mediaPath, 12, 0); err != nil {
		t.Fatalf("mark temporary item ready: %v", err)
	}
	if _, err := store.insertItem(QueueItem{
		ID:            "snapshot-item",
		RoomID:        "room-1",
		Source:        "netease",
		TrackID:       "saved-track",
		Title:         "Saved snapshot",
		Status:        StatusPending,
		AddedByUserID: "user-1",
		QueueScope:    QueueScopeSavedPlaylistSnapshot,
		SnapshotID:    "snapshot-1",
		SortOrder:     10,
	}); err != nil {
		t.Fatalf("insert snapshot item: %v", err)
	}
	state, err := store.ensureState("room-1")
	if err != nil {
		t.Fatalf("ensure state: %v", err)
	}
	state.State = StatePaused
	state.CurrentItemID = "snapshot-item"
	state.PositionMS = 1234
	state.Revision = 7
	state.PlaybackMode = ModeShuffle
	state.ActiveSourceType = ActiveSourceUserPlaylist
	state.ActivePlaylistID = "saved-playlist"
	state.ActivePlaylistName = "Saved"
	state.ActivePlaylistOwnerID = "user-1"
	state.ActivePlaylistCreatedAt = nowMillis()
	state.ActiveSnapshotID = "snapshot-1"
	if err := store.saveState(*state); err != nil {
		t.Fatalf("save active state: %v", err)
	}

	var occupied atomic.Bool
	changed := make(chan struct{}, 1)
	manager := &Manager{
		cfg: Config{
			Enabled:              true,
			EmptyRoomGracePeriod: 30 * time.Millisecond,
		},
		store:                store,
		players:              map[string]*player{},
		seenCommands:         map[string]map[string]int64{},
		playCursors:          map[string]playCursor{},
		emptyRoomTimers:      map[string]*time.Timer{},
		emptyRoomGenerations: map[string]uint64{},
		onRoomChanged:        func(string) { changed <- struct{}{} },
		roomOccupied: func(string) (bool, error) {
			return occupied.Load(), nil
		},
	}

	manager.ObserveRoomOccupancy("room-1", 0)
	occupied.Store(true)
	manager.ObserveRoomOccupancy("room-1", 1)
	time.Sleep(80 * time.Millisecond)
	queue, err := store.listQueue("room-1")
	if err != nil {
		t.Fatalf("list queue after reentry: %v", err)
	}
	if len(queue) != 2 {
		t.Fatalf("reentry did not cancel cleanup: queue length = %d", len(queue))
	}

	occupied.Store(false)
	manager.ObserveRoomOccupancy("room-1", 0)
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("empty-room cleanup did not finish")
	}
	queue, err = store.listQueue("room-1")
	if err != nil {
		t.Fatalf("list queue after expiry: %v", err)
	}
	if len(queue) != 0 {
		t.Fatalf("transient queue length = %d, want 0", len(queue))
	}
	if _, err := os.Stat(mediaPath); !os.IsNotExist(err) {
		t.Fatalf("temporary media was not released: %v", err)
	}
	var savedPlaylistCount int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM music_playlists WHERE id = ?`,
		"saved-playlist",
	).Scan(&savedPlaylistCount); err != nil {
		t.Fatalf("count saved playlist: %v", err)
	}
	if savedPlaylistCount != 1 {
		t.Fatalf("saved playlist count = %d, want 1", savedPlaylistCount)
	}
	state, err = store.getState("room-1")
	if err != nil {
		t.Fatalf("read expired state: %v", err)
	}
	if state.State != StateStopped || state.CurrentItemID != "" ||
		state.PositionMS != 0 || state.ActiveSourceType != ActiveSourceTemporary ||
		state.ActivePlaylistID != "" || state.ActiveSnapshotID != "" ||
		state.PlaybackMode != ModeSequential || state.Revision != 8 {
		t.Fatalf("expired state = %#v", state)
	}
}

func TestRepeatedEmptyObservationsDoNotExtendGracePeriod(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.insertItem(QueueItem{
		ID:            "temporary-item",
		RoomID:        "room-1",
		Source:        "netease",
		TrackID:       "temporary-track",
		Title:         "Temporary",
		Status:        StatusPending,
		AddedByUserID: "user-1",
		QueueScope:    QueueScopeTemporary,
		SortOrder:     10,
	}); err != nil {
		t.Fatalf("insert temporary item: %v", err)
	}
	if _, err := store.ensureState("room-1"); err != nil {
		t.Fatalf("ensure state: %v", err)
	}
	changed := make(chan struct{}, 1)
	manager := &Manager{
		cfg: Config{
			Enabled:              true,
			EmptyRoomGracePeriod: 60 * time.Millisecond,
		},
		store:                store,
		players:              map[string]*player{},
		seenCommands:         map[string]map[string]int64{},
		playCursors:          map[string]playCursor{},
		emptyRoomTimers:      map[string]*time.Timer{},
		emptyRoomGenerations: map[string]uint64{},
		onRoomChanged:        func(string) { changed <- struct{}{} },
		roomOccupied:         func(string) (bool, error) { return false, nil },
	}

	manager.ObserveRoomOccupancy("room-1", 0)
	manager.emptyRoomMu.Lock()
	firstTimer := manager.emptyRoomTimers["room-1"]
	manager.emptyRoomMu.Unlock()
	if firstTimer == nil {
		t.Fatal("initial empty observation did not start a timer")
	}
	time.Sleep(40 * time.Millisecond)
	manager.ObserveRoomOccupancy("room-1", 0)
	manager.emptyRoomMu.Lock()
	secondTimer := manager.emptyRoomTimers["room-1"]
	manager.emptyRoomMu.Unlock()
	if secondTimer != firstTimer {
		t.Fatal("repeated empty observation replaced and extended the timer")
	}
	select {
	case <-changed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("empty-room cleanup did not finish")
	}
}
