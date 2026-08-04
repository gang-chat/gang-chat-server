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
