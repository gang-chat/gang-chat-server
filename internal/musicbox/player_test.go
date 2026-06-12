package musicbox

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestPlayer builds a player wired only with the fields the heartbeat needs,
// skipping connect()/run() so no LiveKit connection is involved.
func newTestPlayer(onState func()) *player {
	return &player{
		roomID:  "room-1",
		cmd:     make(chan command, 8),
		done:    make(chan struct{}),
		onState: onState,
	}
}

func TestHeartbeatFansOutWhilePlaying(t *testing.T) {
	var calls int32
	p := newTestPlayer(func() { atomic.AddInt32(&calls, 1) })
	// Actively playing: running, not paused, with a current item.
	p.current = &QueueItem{ID: "item-1"}

	go p.heartbeat()
	defer p.doneOnce.Do(func() { close(p.done) })

	// Over ~2.5s the 1s ticker should fire about twice.
	time.Sleep(2500 * time.Millisecond)
	got := atomic.LoadInt32(&calls)
	if got < 2 {
		t.Fatalf("expected at least 2 heartbeat fan-outs while playing, got %d", got)
	}
}

func TestHeartbeatSilentWhilePaused(t *testing.T) {
	var calls int32
	p := newTestPlayer(func() { atomic.AddInt32(&calls, 1) })
	p.current = &QueueItem{ID: "item-1"}
	p.paused = true

	go p.heartbeat()
	defer p.doneOnce.Do(func() { close(p.done) })

	time.Sleep(2500 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("expected no fan-outs while paused, got %d", got)
	}
}

func TestHeartbeatSilentWithoutCurrentItem(t *testing.T) {
	var calls int32
	p := newTestPlayer(func() { atomic.AddInt32(&calls, 1) })
	// Running, not paused, but nothing loaded yet.

	go p.heartbeat()
	defer p.doneOnce.Do(func() { close(p.done) })

	time.Sleep(2500 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("expected no fan-outs without a current item, got %d", got)
	}
}

func TestHeartbeatExitsOnDone(t *testing.T) {
	var calls int32
	p := newTestPlayer(func() { atomic.AddInt32(&calls, 1) })
	p.current = &QueueItem{ID: "item-1"}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.heartbeat()
	}()

	// Let it tick once, then signal shutdown and confirm the goroutine returns.
	time.Sleep(1200 * time.Millisecond)
	p.doneOnce.Do(func() { close(p.done) })

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not exit after done was closed")
	}

	// No further fan-outs after shutdown.
	after := atomic.LoadInt32(&calls)
	time.Sleep(1500 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != after {
		t.Fatalf("heartbeat kept firing after done: %d -> %d", after, got)
	}
}
