package musicbox

import (
	"context"
	"errors"
	"testing"
)

func TestEnqueueRejectsDuplicateTrackInTemporaryQueue(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.insertItem(QueueItem{
		ID:            "ready-cap-blocker",
		RoomID:        "r1",
		Source:        "netease",
		TrackID:       "other-track",
		Title:         "Other track",
		Status:        StatusReady,
		FileSizeBytes: 1,
		AddedByUserID: "u1",
		SortOrder:     10,
	}); err != nil {
		t.Fatalf("insert ready cap blocker: %v", err)
	}
	manager := &Manager{
		cfg:          Config{Enabled: true, MaxBytesPerRoom: 1},
		store:        store,
		players:      map[string]*player{},
		seenCommands: map[string]map[string]int64{},
		playCursors:  map[string]playCursor{},
	}
	params := EnqueueParams{
		RoomID:        "r1",
		Source:        "netease",
		TrackID:       "same-track",
		Title:         "Same track",
		AddedByUserID: "u1",
	}
	if _, err := manager.Enqueue(context.Background(), params); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if _, err := manager.Enqueue(
		context.Background(),
		params,
	); !errors.Is(err, ErrQueueItemAlreadyExists) {
		t.Fatalf("duplicate enqueue error = %v, want ErrQueueItemAlreadyExists", err)
	}
	queue, err := store.listScopedQueue("r1", QueueScopeTemporary, "")
	if err != nil {
		t.Fatalf("list temporary queue: %v", err)
	}
	if len(queue) != 2 {
		t.Fatalf("temporary queue length = %d, want 2", len(queue))
	}
}

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

func TestPlayNowReusesPlayerAndPublishesTargetBeforeAcknowledgement(t *testing.T) {
	store := newTestStore(t)
	state, err := store.ensureState("r1")
	if err != nil {
		t.Fatalf("ensure state: %v", err)
	}
	oldItem := add(t, store, "old", 10)
	middleItem := add(t, store, "middle", 20)
	targetItem := add(t, store, "target", 30)
	if err := store.markReady(oldItem.ID, "/tmp/old.ogg", 1, 1000); err != nil {
		t.Fatalf("mark old ready: %v", err)
	}
	if err := store.markReady(middleItem.ID, "/tmp/middle.ogg", 1, 1000); err != nil {
		t.Fatalf("mark middle ready: %v", err)
	}
	if err := store.markReady(targetItem.ID, "/tmp/target.ogg", 1, 1000); err != nil {
		t.Fatalf("mark target ready: %v", err)
	}
	state.CurrentItemID = oldItem.ID
	state.State = StatePlaying
	initialRevision := state.Revision
	if err := store.saveState(*state); err != nil {
		t.Fatalf("save initial state: %v", err)
	}

	oldPlayer := newTestPlayer(nil)
	oldPlayer.roomID = "r1"
	oldPlayer.current = oldItem
	oldPlayer.positionMS = 7250
	oldPlayer.paused = true
	tokenCalls := 0
	manager := &Manager{
		cfg:          Config{Enabled: true},
		store:        store,
		players:      map[string]*player{"r1": oldPlayer},
		seenCommands: map[string]map[string]int64{},
		playCursors:  map[string]playCursor{},
		tokenFn: func(_, _ string) (string, error) {
			tokenCalls++
			return "", errors.New("priority play must not reconnect")
		},
	}
	oldPlayer.onPriorityState = func() { manager.persistAndNotifyForced("r1") }
	go func() {
		command := <-oldPlayer.cmd
		if command.kind != cmdPlayNow {
			t.Errorf("command kind = %v, want cmdPlayNow", command.kind)
		}
		if command.item == nil || command.item.ID != targetItem.ID {
			t.Errorf("command item = %+v, want %s", command.item, targetItem.ID)
		}
		if command.item != nil && command.item.SortOrder != 20 {
			t.Errorf("target sort order = %d, want 20", command.item.SortOrder)
		}
		oldPlayer.setPriorityCurrent(command.item)
		acknowledge(command.ack)
	}()

	if err := manager.ApplyItemControl(
		"r1", "play_now", targetItem.ID, "", "", nil,
	); err != nil {
		t.Fatalf("play now: %v", err)
	}
	if tokenCalls != 0 {
		t.Fatalf("token calls = %d, want 0", tokenCalls)
	}
	if got := manager.getPlayer("r1"); got != oldPlayer {
		t.Fatalf("player = %p, want reused %p", got, oldPlayer)
	}
	playbackState, currentID, positionMS := oldPlayer.snapshot()
	if playbackState != StatePlaying || currentID != targetItem.ID || positionMS != 0 {
		t.Fatalf(
			"player snapshot = (%q, %q, %d), want playing/%s/0",
			playbackState,
			currentID,
			positionMS,
			targetItem.ID,
		)
	}
	updated, err := store.getState("r1")
	if err != nil {
		t.Fatalf("load updated state: %v", err)
	}
	if updated.CurrentItemID != targetItem.ID || updated.State != StatePlaying || updated.PositionMS != 0 {
		t.Fatalf("unexpected priority-play state: %+v", updated)
	}
	if updated.Revision <= initialRevision {
		t.Fatalf("revision = %d, want > %d", updated.Revision, initialRevision)
	}
	queue, err := store.listScopedQueue("r1", QueueScopeTemporary, "")
	if err != nil {
		t.Fatalf("list reordered queue: %v", err)
	}
	if got := queueItemIDs(queue); !equalQueueOrder(
		got,
		[]string{oldItem.ID, targetItem.ID, middleItem.ID},
	) {
		t.Fatalf("queue order = %v, want old/target/middle", got)
	}
	if next := manager.nextItem(
		"r1",
		queue[1],
		transitionNext,
		0,
	); next == nil || next.ID != middleItem.ID {
		t.Fatalf("next item after priority target = %+v, want middle", next)
	}
}

func TestPlayNowKeepsSavedPlaylistOrder(t *testing.T) {
	store := newTestStore(t)
	state, err := store.ensureState("r1")
	if err != nil {
		t.Fatalf("ensure state: %v", err)
	}
	insertSaved := func(id string, sortOrder int64) *QueueItem {
		item, err := store.insertItem(QueueItem{
			ID:            id,
			RoomID:        "r1",
			Source:        "netease",
			TrackID:       "track-" + id,
			Title:         id,
			Status:        StatusReady,
			AddedByUserID: "u1",
			QueueScope:    QueueScopeSavedPlaylistSnapshot,
			SnapshotID:    "snapshot-1",
			SortOrder:     sortOrder,
		})
		if err != nil {
			t.Fatalf("insert saved item %s: %v", id, err)
		}
		return item
	}
	oldItem := insertSaved("saved-old", 10)
	betweenItem := insertSaved("saved-between", 20)
	targetItem := insertSaved("saved-target", 30)
	state.ActiveSourceType = ActiveSourceRoomPlaylist
	state.ActivePlaylistID = "playlist-1"
	state.ActiveSnapshotID = "snapshot-1"
	state.CurrentItemID = oldItem.ID
	state.State = StatePlaying
	if err := store.saveState(*state); err != nil {
		t.Fatalf("save saved-playlist state: %v", err)
	}

	oldPlayer := newTestPlayer(nil)
	oldPlayer.roomID = "r1"
	oldPlayer.current = oldItem
	manager := &Manager{
		cfg:          Config{Enabled: true},
		store:        store,
		players:      map[string]*player{"r1": oldPlayer},
		seenCommands: map[string]map[string]int64{},
		playCursors:  map[string]playCursor{},
	}
	oldPlayer.onPriorityState = func() { manager.persistAndNotifyForced("r1") }
	go func() {
		command := <-oldPlayer.cmd
		if command.item == nil || command.item.SortOrder != targetItem.SortOrder {
			t.Errorf("saved target changed before playback: %+v", command.item)
		}
		oldPlayer.setPriorityCurrent(command.item)
		acknowledge(command.ack)
	}()

	if err := manager.ApplyItemControl(
		"r1", "play_now", targetItem.ID, "", "", nil,
	); err != nil {
		t.Fatalf("play saved item now: %v", err)
	}
	queue, err := store.listScopedQueue(
		"r1",
		QueueScopeSavedPlaylistSnapshot,
		"snapshot-1",
	)
	if err != nil {
		t.Fatalf("list saved queue: %v", err)
	}
	if got := queueItemIDs(queue); !equalQueueOrder(
		got,
		[]string{oldItem.ID, betweenItem.ID, targetItem.ID},
	) {
		t.Fatalf("saved queue order = %v, want unchanged", got)
	}
}

func TestPlayNowConnectionFailureDoesNotPersistTarget(t *testing.T) {
	store := newTestStore(t)
	state, err := store.ensureState("r1")
	if err != nil {
		t.Fatalf("ensure state: %v", err)
	}
	targetItem := add(t, store, "target", 20)
	oldItem := add(t, store, "old", 10)
	betweenItem := add(t, store, "between", 15)
	if err := store.markReady(targetItem.ID, "/tmp/target.ogg", 1, 1000); err != nil {
		t.Fatalf("mark target ready: %v", err)
	}
	initialRevision := state.Revision
	state.CurrentItemID = oldItem.ID
	state.State = StatePlaying
	if err := store.saveState(*state); err != nil {
		t.Fatalf("save playing state: %v", err)
	}
	connectError := errors.New("livekit token unavailable")
	manager := &Manager{
		cfg:          Config{Enabled: true},
		store:        store,
		players:      map[string]*player{},
		seenCommands: map[string]map[string]int64{},
		playCursors:  map[string]playCursor{},
		tokenFn: func(_, _ string) (string, error) {
			return "", connectError
		},
	}

	if err := manager.ApplyItemControl(
		"r1", "play_now", targetItem.ID, "", "", nil,
	); !errors.Is(err, connectError) {
		t.Fatalf("play now error = %v, want %v", err, connectError)
	}
	if got := manager.getPlayer("r1"); got != nil {
		t.Fatalf("player = %p, want nil", got)
	}
	updated, err := store.getState("r1")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if updated.CurrentItemID != state.CurrentItemID ||
		updated.State != state.State ||
		updated.PositionMS != state.PositionMS ||
		updated.Revision != initialRevision {
		t.Fatalf("failed priority play mutated state: before=%+v after=%+v", state, updated)
	}
	queue, err := store.listScopedQueue("r1", QueueScopeTemporary, "")
	if err != nil {
		t.Fatalf("list restored queue: %v", err)
	}
	if got := queueItemIDs(queue); !equalQueueOrder(
		got,
		[]string{oldItem.ID, betweenItem.ID, targetItem.ID},
	) {
		t.Fatalf("queue order after failed play now = %v", got)
	}
}

func queueItemIDs(items []*QueueItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func equalQueueOrder(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestClearTemporaryPlaylistKeepsSavedSnapshotsAndIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	state, err := store.ensureState("r1")
	if err != nil {
		t.Fatalf("ensure state: %v", err)
	}
	temporary := add(t, store, "temporary", 10)
	if err := store.markReady(temporary.ID, "", 0, 1000); err != nil {
		t.Fatalf("mark temporary ready: %v", err)
	}
	saved, err := store.insertItem(QueueItem{
		ID:            "saved",
		RoomID:        "r1",
		Source:        "netease",
		TrackID:       "saved-track",
		Title:         "Saved",
		Status:        StatusReady,
		AddedByUserID: "u1",
		QueueScope:    QueueScopeSavedPlaylistSnapshot,
		SnapshotID:    "snapshot-1",
		SortOrder:     20,
	})
	if err != nil {
		t.Fatalf("insert saved snapshot: %v", err)
	}
	initialRevision := state.Revision
	manager := &Manager{
		cfg:          Config{Enabled: true},
		store:        store,
		players:      map[string]*player{},
		seenCommands: map[string]map[string]int64{},
		playCursors:  map[string]playCursor{},
	}

	if err := manager.ApplyItemControl(
		"r1",
		"clear_temporary_playlist",
		"",
		"",
		"clear-1",
		&initialRevision,
	); err != nil {
		t.Fatalf("clear temporary playlist: %v", err)
	}
	temporaryItems, err := store.listScopedQueue("r1", QueueScopeTemporary, "")
	if err != nil {
		t.Fatalf("list temporary queue: %v", err)
	}
	if len(temporaryItems) != 0 {
		t.Fatalf("temporary queue length = %d, want 0", len(temporaryItems))
	}
	savedItems, err := store.listScopedQueue(
		"r1",
		QueueScopeSavedPlaylistSnapshot,
		"snapshot-1",
	)
	if err != nil {
		t.Fatalf("list saved snapshot: %v", err)
	}
	if len(savedItems) != 1 || savedItems[0].ID != saved.ID {
		t.Fatalf("saved snapshot changed: %+v", savedItems)
	}
	updated, err := store.getState("r1")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if updated.Revision != initialRevision+1 {
		t.Fatalf("revision = %d, want %d", updated.Revision, initialRevision+1)
	}

	// A retried confirmation carries the same command ID and must not bump the
	// revision or clear anything a second time.
	if err := manager.ApplyItemControl(
		"r1",
		"clear_temporary_playlist",
		"",
		"",
		"clear-1",
		&initialRevision,
	); err != nil {
		t.Fatalf("idempotent clear retry: %v", err)
	}
	retried, err := store.getState("r1")
	if err != nil {
		t.Fatalf("load retried state: %v", err)
	}
	if retried.Revision != updated.Revision {
		t.Fatalf("retry revision = %d, want %d", retried.Revision, updated.Revision)
	}
}
