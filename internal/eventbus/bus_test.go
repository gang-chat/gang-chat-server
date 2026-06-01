package eventbus

import (
	"testing"
	"time"
)

// drain reads one event from sub with a timeout, failing if none arrives.
func recv(t *testing.T, sub *Subscription) Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatalf("subscription closed unexpectedly")
		}
		return ev
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for event")
		return Event{}
	}
}

// expectNone asserts no event is queued for sub.
func expectNone(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case ev := <-sub.Events():
		t.Fatalf("expected no event, got %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPublishUserReachesAllConnections(t *testing.T) {
	b := New()
	// Two devices for the same user, plus an unrelated user.
	dev1 := b.Subscribe("alice")
	dev2 := b.Subscribe("alice")
	other := b.Subscribe("bob")
	defer dev1.Close()
	defer dev2.Close()
	defer other.Close()

	b.PublishUser("alice", Event{Type: "room_added", RoomID: "room1"})

	if got := recv(t, dev1); got.Type != "room_added" {
		t.Fatalf("dev1 got %q", got.Type)
	}
	if got := recv(t, dev2); got.Type != "room_added" {
		t.Fatalf("dev2 got %q", got.Type)
	}
	expectNone(t, other)
}

// A user with no room interest still receives PublishUser events — this is the
// property that lets a freshly approved member learn they joined a room their
// connection never subscribed to.
func TestPublishUserIgnoresRoomInterest(t *testing.T) {
	b := New()
	sub := b.Subscribe("alice") // never calls SetRooms
	defer sub.Close()

	b.PublishUser("alice", Event{Type: "room_added", RoomID: "room1"})

	if got := recv(t, sub); got.RoomID != "room1" {
		t.Fatalf("got room %q", got.RoomID)
	}
}

func TestPublishUserAfterCloseIsDropped(t *testing.T) {
	b := New()
	sub := b.Subscribe("alice")
	sub.Close()

	// Must not panic or block now that the connection is gone.
	b.PublishUser("alice", Event{Type: "room_updated", RoomID: "room1"})

	if _, ok := b.byUser["alice"]; ok {
		t.Fatalf("byUser should have no entry for alice after close")
	}
}

func TestPublishRoomStillScopedToInterest(t *testing.T) {
	b := New()
	in := b.Subscribe("alice")
	out := b.Subscribe("bob")
	defer in.Close()
	defer out.Close()
	in.SetRooms([]string{"room1"})

	b.PublishRoom("room1", Event{Type: "live_participant_joined"})

	if got := recv(t, in); got.Type != "live_participant_joined" {
		t.Fatalf("interested sub got %q", got.Type)
	}
	expectNone(t, out)
}

func TestCloseCleansUpIndexes(t *testing.T) {
	b := New()
	sub := b.Subscribe("alice")
	sub.SetRooms([]string{"room1"})
	sub.Close()

	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.byID) != 0 {
		t.Fatalf("byID not cleaned: %d", len(b.byID))
	}
	if len(b.byUser) != 0 {
		t.Fatalf("byUser not cleaned: %d", len(b.byUser))
	}
	if len(b.byRoom) != 0 {
		t.Fatalf("byRoom not cleaned: %d", len(b.byRoom))
	}
}
