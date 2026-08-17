package musicbox

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestEnsurePlayingDoesNotHoldGlobalPlayerLockDuringStartup(t *testing.T) {
	store := newTestStore(t)
	item := add(t, store, "ready", 10)
	if err := store.markReady(item.ID, "/tmp/ready.ogg", 1, 1000); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	tokenStarted := make(chan struct{})
	releaseToken := make(chan struct{})
	tokenErr := errors.New("token generation stopped for test")
	manager := &Manager{
		cfg:          Config{Enabled: true},
		store:        store,
		players:      map[string]*player{},
		seenCommands: map[string]map[string]int64{},
		playCursors:  map[string]playCursor{},
		tokenFn: func(_, _ string) (string, error) {
			close(tokenStarted)
			<-releaseToken
			return "", tokenErr
		},
	}

	result := make(chan error, 1)
	go func() { result <- manager.ensurePlaying("r1") }()
	select {
	case <-tokenStarted:
	case <-time.After(time.Second):
		t.Fatal("player startup deadlocked before token generation")
	}

	playerRead := make(chan *player, 1)
	go func() { playerRead <- manager.getPlayer("r1") }()
	select {
	case player := <-playerRead:
		if player != nil {
			t.Fatalf("player became visible before startup completed: %p", player)
		}
	case <-time.After(time.Second):
		t.Fatal("global player lock was held during token generation")
	}

	close(releaseToken)
	select {
	case err := <-result:
		if !errors.Is(err, tokenErr) {
			t.Fatalf("ensure playing error = %v, want %v", err, tokenErr)
		}
	case <-time.After(time.Second):
		t.Fatal("player startup did not finish after token generation returned")
	}
}

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

func TestEnqueueRejectsTracksBeyondTemporaryQueueLimit(t *testing.T) {
	store := newTestStore(t)
	for index := 0; index < MaxTemporaryQueueItems; index++ {
		if _, err := store.insertItem(QueueItem{
			ID:            fmt.Sprintf("queue-limit-%03d", index),
			RoomID:        "r1",
			Source:        "netease",
			TrackID:       fmt.Sprintf("track-%03d", index),
			Title:         "Queued track",
			Status:        StatusPending,
			AddedByUserID: "u1",
			SortOrder:     int64(index + 1),
		}); err != nil {
			t.Fatalf("insert queue item %d: %v", index, err)
		}
	}
	manager := &Manager{
		cfg:          Config{Enabled: true, MaxBytesPerRoom: 1},
		store:        store,
		players:      map[string]*player{},
		seenCommands: map[string]map[string]int64{},
		playCursors:  map[string]playCursor{},
	}

	_, err := manager.Enqueue(context.Background(), EnqueueParams{
		RoomID:        "r1",
		Source:        "netease",
		TrackID:       "track-over-limit",
		Title:         "Track over limit",
		AddedByUserID: "u1",
	})
	if !errors.Is(err, ErrQueueLimitReached) {
		t.Fatalf("enqueue error = %v, want ErrQueueLimitReached", err)
	}
	queue, listErr := store.listScopedQueue("r1", QueueScopeTemporary, "")
	if listErr != nil {
		t.Fatalf("list temporary queue: %v", listErr)
	}
	if len(queue) != MaxTemporaryQueueItems {
		t.Fatalf("temporary queue length = %d, want %d", len(queue), MaxTemporaryQueueItems)
	}
}

func TestTemporaryPlaybackHistoryKeepsOnlyTheLatestTwentyPlayedRows(t *testing.T) {
	store := newTestStore(t)
	manager := &Manager{
		cfg:          Config{Enabled: true, Dir: t.TempDir(), CacheMaxBytes: 1 << 20},
		store:        store,
		players:      map[string]*player{},
		seenCommands: map[string]map[string]int64{},
		playCursors:  map[string]playCursor{},
		cacheLeases:  map[string]int{},
	}
	var lastPlayed *QueueItem
	for index := 1; index <= MaxTemporaryPlaybackHistory+7; index++ {
		item, err := store.insertItem(QueueItem{
			ID: fmt.Sprintf("history-%d", index), RoomID: "r1",
			Source: "netease", TrackID: fmt.Sprintf("track-%d", index),
			Title: "Track", Status: StatusPending, SortOrder: int64(index * 10),
			QueueScope: QueueScopeTemporary,
		})
		if err != nil {
			t.Fatalf("insert history row %d: %v", index, err)
		}
		if err := store.markReady(item.ID, "", 0, 1000); err != nil {
			t.Fatalf("mark history row %d ready: %v", index, err)
		}
		reloaded, err := store.getItem(item.ID)
		if err != nil {
			t.Fatalf("reload history row %d: %v", index, err)
		}
		if index == MaxTemporaryPlaybackHistory+5 {
			lastPlayed = reloaded
		}
	}
	state, err := store.ensureState("r1")
	if err != nil {
		t.Fatalf("ensure state: %v", err)
	}
	state.ActiveSourceType = ActiveSourceTemporary
	if err := store.saveState(*state); err != nil {
		t.Fatalf("save state: %v", err)
	}

	if next := manager.nextItem("r1", lastPlayed, transitionNatural, 1000); next == nil || next.ID != "history-26" {
		t.Fatalf("next item = %+v, want history-26", next)
	}
	remaining, err := store.listScopedQueue("r1", QueueScopeTemporary, "")
	if err != nil {
		t.Fatalf("list remaining history: %v", err)
	}
	if len(remaining) != MaxTemporaryPlaybackHistory+2 {
		t.Fatalf("remaining rows = %d, want %d", len(remaining), MaxTemporaryPlaybackHistory+2)
	}
	if remaining[0].ID != "history-6" || remaining[len(remaining)-1].ID != "history-27" {
		t.Fatalf(
			"unexpected retained history range: first=%q last=%q",
			remaining[0].ID,
			remaining[len(remaining)-1].ID,
		)
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

func TestActivateTemporaryAtPendingItemDoesNotExposePartialSourceSwitch(t *testing.T) {
	store := newTestStore(t)
	state, err := store.ensureState("r1")
	if err != nil {
		t.Fatalf("ensure state: %v", err)
	}
	state.ActiveSourceType = ActiveSourceRoomPlaylist
	state.ActivePlaylistID = "room-list"
	state.ActiveSnapshotID = "snapshot-1"
	if err := store.saveState(*state); err != nil {
		t.Fatalf("save active state: %v", err)
	}
	pending := add(t, store, "pending", 10)
	manager := &Manager{
		cfg:          Config{Enabled: true},
		store:        store,
		players:      map[string]*player{},
		seenCommands: map[string]map[string]int64{},
		playCursors:  map[string]playCursor{},
	}

	err = manager.ActivatePlaylist(
		"r1",
		ActiveSourceTemporary,
		"",
		"点歌队列",
		"",
		0,
		"u1",
		nil,
		true,
		pending.ID,
		-1,
	)
	if !errors.Is(err, ErrQueueItemNotReady) {
		t.Fatalf("activate pending item error = %v, want ErrQueueItemNotReady", err)
	}
	after, err := store.getState("r1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if after.ActiveSourceType != ActiveSourceRoomPlaylist ||
		after.ActivePlaylistID != "room-list" ||
		after.ActiveSnapshotID != "snapshot-1" {
		t.Fatalf("activation exposed partial state: %+v", after)
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

func TestControlActionRevisionGuardPolicy(t *testing.T) {
	transportActions := []string{
		"play",
		"resume",
		"pause",
		"skip",
		"next",
		"previous",
		"play_now",
		"stop",
	}
	for _, action := range transportActions {
		if controlActionRequiresRevisionGuard(action) {
			t.Fatalf("transport action %q unexpectedly requires a revision", action)
		}
	}
	for _, action := range []string{"clear_temporary_playlist", "set_mode"} {
		if !controlActionRequiresRevisionGuard(action) {
			t.Fatalf("structural action %q must require a revision", action)
		}
	}
}

func TestTransportControlIgnoresStaleClientRevision(t *testing.T) {
	store := newTestStore(t)
	state, err := store.ensureState("r1")
	if err != nil {
		t.Fatalf("ensure state: %v", err)
	}
	staleRevision := state.Revision
	state.Revision++
	if err := store.saveState(*state); err != nil {
		t.Fatalf("advance revision: %v", err)
	}
	manager := &Manager{
		cfg:          Config{Enabled: true},
		store:        store,
		players:      map[string]*player{},
		seenCommands: map[string]map[string]int64{},
		playCursors:  map[string]playCursor{},
	}

	if err := manager.ApplyControl(
		"r1",
		"pause",
		"",
		"pause-stale-client",
		&staleRevision,
	); err != nil {
		t.Fatalf("stale transport control: %v", err)
	}
	if err := manager.ApplyControl(
		"r1",
		"set_mode",
		string(ModeRepeatOne),
		"mode-stale-client",
		&staleRevision,
	); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale structural control error = %v, want ErrRevisionConflict", err)
	}
}
