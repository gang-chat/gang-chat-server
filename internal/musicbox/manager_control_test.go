package musicbox

import (
	"errors"
	"testing"
)

func TestPlayNowRejectsMissingPendingAndInactiveItems(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.ensureState("r1"); err != nil {
		t.Fatalf("ensure state: %v", err)
	}
	manager := &Manager{
		cfg:          Config{Enabled: true},
		store:        store,
		players:      map[string]*player{},
		seenCommands: map[string]map[string]int64{},
		playCursors:  map[string]playCursor{},
	}

	if err := manager.ApplyItemControl(
		"r1", "play_now", "missing", "", "", nil,
	); !errors.Is(err, ErrQueueItemNotFound) {
		t.Fatalf("missing item error = %v, want ErrQueueItemNotFound", err)
	}

	pending := add(t, store, "pending", 10)
	if err := manager.ApplyItemControl(
		"r1", "play_now", pending.ID, "", "", nil,
	); !errors.Is(err, ErrQueueItemNotReady) {
		t.Fatalf("pending item error = %v, want ErrQueueItemNotReady", err)
	}

	inactive, err := store.insertItem(QueueItem{
		ID:            "inactive",
		RoomID:        "r1",
		Source:        "netease",
		TrackID:       "inactive-track",
		Title:         "Inactive",
		Status:        StatusReady,
		AddedByUserID: "u1",
		QueueScope:    QueueScopeSavedPlaylistSnapshot,
		SnapshotID:    "snapshot-1",
		SortOrder:     20,
	})
	if err != nil {
		t.Fatalf("insert inactive item: %v", err)
	}
	if err := manager.ApplyItemControl(
		"r1", "play_now", inactive.ID, "", "", nil,
	); !errors.Is(err, ErrQueueItemNotFound) {
		t.Fatalf("inactive item error = %v, want ErrQueueItemNotFound", err)
	}
}

func TestPlayNowWaitsForRunLoopCleanupBeforeStartingReplacement(t *testing.T) {
	store := newTestStore(t)
	state, err := store.ensureState("r1")
	if err != nil {
		t.Fatalf("ensure state: %v", err)
	}
	oldItem := add(t, store, "old", 10)
	targetItem := add(t, store, "target", 20)
	if err := store.markReady(oldItem.ID, "/tmp/old.ogg", 1, 1000); err != nil {
		t.Fatalf("mark old ready: %v", err)
	}
	if err := store.markReady(targetItem.ID, "/tmp/target.ogg", 1, 1000); err != nil {
		t.Fatalf("mark target ready: %v", err)
	}
	state.CurrentItemID = oldItem.ID
	state.State = StatePlaying
	if err := store.saveState(*state); err != nil {
		t.Fatalf("save initial state: %v", err)
	}

	oldPlayer := newTestPlayer(nil)
	oldPlayer.roomID = "r1"
	oldPlayer.current = oldItem
	replacementStartError := errors.New("replacement start reached")
	manager := &Manager{
		cfg:          Config{Enabled: true},
		store:        store,
		players:      map[string]*player{"r1": oldPlayer},
		seenCommands: map[string]map[string]int64{},
		playCursors:  map[string]playCursor{},
		tokenFn: func(_, _ string) (string, error) {
			return "", replacementStartError
		},
	}
	go func() {
		command := <-oldPlayer.cmd
		acknowledge(command.ack)
		oldPlayer.disconnect()
		manager.mu.Lock()
		if manager.players["r1"] == oldPlayer {
			delete(manager.players, "r1")
		}
		manager.mu.Unlock()
	}()

	if err := manager.ApplyItemControl(
		"r1", "play_now", targetItem.ID, "", "", nil,
	); !errors.Is(err, replacementStartError) {
		t.Fatalf("play now error = %v, want replacement start error", err)
	}
	if got := manager.getPlayer("r1"); got != nil {
		t.Fatal("replacement start was attempted before old player cleanup")
	}
	updated, err := store.getState("r1")
	if err != nil {
		t.Fatalf("load updated state: %v", err)
	}
	if updated.CurrentItemID != targetItem.ID || updated.State != StateStopped {
		t.Fatalf("unexpected priority-play state: %+v", updated)
	}
}
