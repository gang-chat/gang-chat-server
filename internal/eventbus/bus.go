// Package eventbus implements an in-memory publish/subscribe channel for
// fan-out of room-scoped events to active client connections (typically a
// long-lived SSE stream on /api/v1/me/stream).
//
// This is the single real-time delivery path from server to client. Anyone
// who mutates state that other users care about (HTTP write handlers,
// LiveKit webhook handlers, background jobs) should call Bus.Publish so
// every interested subscriber receives the event.
//
// Subscribers are per-connection rather than per-user: a single user can
// have multiple devices online at once and each one needs its own delivery
// channel.
package eventbus

import (
	"sync"
	"sync/atomic"
)

// Event is what flows through the bus. Everything is JSON-serializable so
// the SSE writer can hand it straight to encoding/json.
type Event struct {
	// Type is the event name, e.g. "live_participants_changed",
	// "message_created", "room_updated". Clients dispatch on this.
	Type string `json:"type"`
	// RoomID is the room the event belongs to. May be empty for non-room
	// events (account-level notifications, etc.) but is set for everything
	// that flows through Bus.PublishRoom.
	RoomID string `json:"room_id,omitempty"`
	// Data is the event payload. Recommended pattern: include a full
	// snapshot of the affected slice of state so clients can blindly
	// overwrite what they have, rather than trying to merge deltas.
	Data any `json:"data,omitempty"`
}

// Subscription is a single connection's view onto the bus. Callers receive
// events on Events() and call Close() (or just let Context cancel) when
// the connection ends. Subscriptions are cheap; create one per HTTP
// request handler.
type Subscription struct {
	id     uint64
	userID string
	bus    *Bus
	events chan Event
	rooms  map[string]struct{}
	mu     sync.Mutex
	closed bool
}

// Events returns the channel events arrive on. The channel is closed when
// the subscription is closed.
func (s *Subscription) Events() <-chan Event { return s.events }

// SetRooms replaces the subscription's interest set. Events published
// to a roomID not in this set will not be delivered to this subscription.
func (s *Subscription) SetRooms(roomIDs []string) {
	next := make(map[string]struct{}, len(roomIDs))
	for _, id := range roomIDs {
		if id == "" {
			continue
		}
		next[id] = struct{}{}
	}
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	for roomID := range s.rooms {
		if _, keep := next[roomID]; keep {
			continue
		}
		if subs, ok := s.bus.byRoom[roomID]; ok {
			delete(subs, s.id)
			if len(subs) == 0 {
				delete(s.bus.byRoom, roomID)
			}
		}
	}
	for roomID := range next {
		if _, had := s.rooms[roomID]; had {
			continue
		}
		subs, ok := s.bus.byRoom[roomID]
		if !ok {
			subs = make(map[uint64]*Subscription)
			s.bus.byRoom[roomID] = subs
		}
		subs[s.id] = s
	}
	s.rooms = next
}

// Close removes the subscription from the bus and closes the events
// channel. Idempotent.
func (s *Subscription) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	s.bus.mu.Lock()
	for roomID := range s.rooms {
		if subs, ok := s.bus.byRoom[roomID]; ok {
			delete(subs, s.id)
			if len(subs) == 0 {
				delete(s.bus.byRoom, roomID)
			}
		}
	}
	if conns, ok := s.bus.byUser[s.userID]; ok {
		delete(conns, s.id)
		if len(conns) == 0 {
			delete(s.bus.byUser, s.userID)
		}
	}
	delete(s.bus.byID, s.id)
	s.bus.mu.Unlock()
	close(s.events)
}

// Bus is the registry of active subscriptions. Concurrency-safe; intended
// to live for the lifetime of the process as a single shared instance.
type Bus struct {
	mu     sync.RWMutex
	nextID atomic.Uint64
	byID   map[uint64]*Subscription
	// byRoom maps roomID -> set of subscriptions interested in that room.
	// Maintained in lockstep with Subscription.rooms so PublishRoom is O(N)
	// in subscribers-of-that-room rather than O(total-subscribers).
	byRoom map[string]map[uint64]*Subscription
	// byUser maps userID -> set of that user's live connections. Unlike
	// byRoom this never depends on room membership: a connection's userID is
	// fixed for its whole lifetime, so this index is the stable way to reach
	// "every device this account has online right now" — which is exactly the
	// audience for account-scoped events (PublishUser). Maintained in lockstep
	// with Subscribe/Close.
	byUser map[string]map[uint64]*Subscription
}

// New creates an empty Bus.
func New() *Bus {
	return &Bus{
		byID:   make(map[uint64]*Subscription),
		byRoom: make(map[string]map[uint64]*Subscription),
		byUser: make(map[string]map[uint64]*Subscription),
	}
}

// Subscribe registers a new subscription owned by userID. The returned
// subscription has no room interests until SetRooms is called.
//
// The buffer is intentionally generous: SSE handlers run a select loop
// that should drain it quickly, but a brief stall (TCP backpressure,
// slow client) shouldn't take down the publishing path. If the buffer
// is full Publish drops the oldest event for that subscription rather
// than blocking the publisher.
func (b *Bus) Subscribe(userID string) *Subscription {
	id := b.nextID.Add(1)
	sub := &Subscription{
		id:     id,
		userID: userID,
		bus:    b,
		events: make(chan Event, 64),
		rooms:  make(map[string]struct{}),
	}
	b.mu.Lock()
	b.byID[id] = sub
	conns, ok := b.byUser[userID]
	if !ok {
		conns = make(map[uint64]*Subscription)
		b.byUser[userID] = conns
	}
	conns[id] = sub
	b.mu.Unlock()
	return sub
}

// OnlineUserIDs returns the set of users that currently have at least one
// active connection registered with the bus. The returned map is detached
// from the bus and safe for callers to read without holding the bus lock.
func (b *Bus) OnlineUserIDs() map[string]struct{} {
	b.mu.RLock()
	defer b.mu.RUnlock()
	ids := make(map[string]struct{}, len(b.byUser))
	for userID, conns := range b.byUser {
		if len(conns) > 0 {
			ids[userID] = struct{}{}
		}
	}
	return ids
}

// PublishRoom delivers ev to every subscription that has roomID in its
// interest set. Best-effort: if a subscription's buffer is full, the
// oldest queued event for that subscription is dropped to make room.
// We never block the publisher.
func (b *Bus) PublishRoom(roomID string, ev Event) {
	if ev.RoomID == "" {
		ev.RoomID = roomID
	}
	b.mu.RLock()
	targets := b.byRoom[roomID]
	if len(targets) == 0 {
		b.mu.RUnlock()
		return
	}
	subs := make([]*Subscription, 0, len(targets))
	for _, sub := range targets {
		subs = append(subs, sub)
	}
	b.mu.RUnlock()
	for _, sub := range subs {
		deliver(sub, ev)
	}
}

// AddUserRoomInterest adds roomID to the interest set of every connection
// currently owned by userID, so they start receiving that room's PublishRoom
// fan-out without reconnecting. This is for the case where interest is gained
// mid-session rather than at connect time — e.g. a superuser ghost who opened
// their SSE stream long ago and only now joins a room's voice channel. A plain
// PublishUser event reaches them regardless of interest, but the ongoing live
// snapshots flow through PublishRoom, which is interest-gated; without this
// they'd hear nothing until the next reconnect re-seeds interest from the DB.
// No-op if the user has no live connections.
func (b *Bus) AddUserRoomInterest(userID, roomID string) {
	if roomID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sub := range b.byUser[userID] {
		if _, had := sub.rooms[roomID]; had {
			continue
		}
		sub.rooms[roomID] = struct{}{}
		subs, ok := b.byRoom[roomID]
		if !ok {
			subs = make(map[uint64]*Subscription)
			b.byRoom[roomID] = subs
		}
		subs[sub.id] = sub
	}
}

// PublishUser delivers ev to every connection owned by userID, regardless
// of room interest. This is the delivery path for account-scoped events:
// membership changes ("you were added to / removed from a room"), "you were
// kicked", "your password changed elsewhere", and so on. It reaches a user's
// connections even for rooms they don't (yet) subscribe to, which is what
// makes it the right tool for telling someone they've just joined a room.
func (b *Bus) PublishUser(userID string, ev Event) {
	b.mu.RLock()
	conns := b.byUser[userID]
	if len(conns) == 0 {
		b.mu.RUnlock()
		return
	}
	subs := make([]*Subscription, 0, len(conns))
	for _, sub := range conns {
		subs = append(subs, sub)
	}
	b.mu.RUnlock()
	for _, sub := range subs {
		deliver(sub, ev)
	}
}

// deliver pushes ev onto sub's channel. If full, drops the oldest item
// (non-blocking) so the publisher never waits on a slow consumer.
func deliver(sub *Subscription, ev Event) {
	sub.mu.Lock()
	if sub.closed {
		sub.mu.Unlock()
		return
	}
	sub.mu.Unlock()
	for {
		select {
		case sub.events <- ev:
			return
		default:
			// Drop oldest to make room. The next reader will see the
			// gap; clients are expected to recover by re-fetching a
			// snapshot on reconnect, so a few dropped fan-out events
			// during backpressure are tolerable.
			select {
			case <-sub.events:
			default:
				// Channel emptied between writes; retry.
			}
		}
	}
}
