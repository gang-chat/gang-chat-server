package musicbox

import (
	"errors"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/server-sdk-go/v2/pkg/oggreader"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// botIdentity is the LiveKit participant identity the music box publishes as.
// Clients can special-case it (e.g. render as a "music" speaker) and it must
// not collide with any real user id.
const botIdentity = "__musicbox__"

// player owns the LiveKit connection and playback loop for one room. It pulls
// the next track from the manager via the advance callback, so queue policy
// lives in the manager and the player just plays what it's handed.
type player struct {
	roomID    string
	host      string
	token     string
	opusTrack *lksdk.LocalTrack

	mu         sync.Mutex
	room       *lksdk.Room
	cmd        chan command
	paused     bool
	stopped    bool
	current    *QueueItem
	positionMS int64

	// done is closed once when the player shuts down, so the heartbeat goroutine
	// that fans out compact per-second position updates can exit.
	done     chan struct{}
	doneOnce sync.Once

	// advance returns the next item to play after the given one finishes (or
	// the first item if prev is nil). Returns nil when the queue is exhausted.
	advance func(prev *QueueItem, transition playbackTransition, positionMS int64) *QueueItem
	// onState is called whenever playback state changes so the manager can
	// persist it and fan out an SSE snapshot.
	onState         func()
	onPriorityState func()
	onHeartbeat     func()
	acquireLease    func(item *QueueItem) func()
}

type commandKind int

const (
	cmdPause commandKind = iota
	cmdResume
	cmdSkip
	cmdPrevious
	cmdPlayNow
	cmdStop
	cmdWake // re-check the queue when idling; no-op during playback
)

type command struct {
	kind commandKind
	ack  chan struct{}
	item *QueueItem
}

type playbackTransition int

const (
	transitionNatural playbackTransition = iota
	transitionNext
	transitionPrevious
)

type playResult struct {
	stop       bool
	transition playbackTransition
	positionMS int64
	ack        chan struct{}
	priority   *QueueItem
}

func newPlayer(
	roomID, host, token string,
	advance func(prev *QueueItem, transition playbackTransition, positionMS int64) *QueueItem,
	onState func(),
	onPriorityState func(),
	onHeartbeat func(),
	acquireLease func(item *QueueItem) func(),
) *player {
	return &player{
		roomID:          roomID,
		host:            host,
		token:           token,
		cmd:             make(chan command, 8),
		done:            make(chan struct{}),
		advance:         advance,
		onState:         onState,
		onPriorityState: onPriorityState,
		onHeartbeat:     onHeartbeat,
		acquireLease:    acquireLease,
	}
}

// connect joins the LiveKit room as the bot and publishes a silent Opus track
// that the playback loop writes samples into.
func (p *player) connect() error {
	room, err := lksdk.ConnectToRoomWithToken(p.host, p.token, &lksdk.RoomCallback{}, lksdk.WithAutoSubscribe(false))
	if err != nil {
		return err
	}
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	})
	if err != nil {
		room.Disconnect()
		return err
	}
	if _, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name:   "music-box",
		Source: livekit.TrackSource_MICROPHONE,
		Stereo: true,
	}); err != nil {
		room.Disconnect()
		return err
	}
	p.mu.Lock()
	p.room = room
	p.opusTrack = track
	p.mu.Unlock()
	return nil
}

// run is the playback loop. It plays items handed back by advance until the
// queue is exhausted or stop is requested, then disconnects.
func (p *player) run() {
	p.runFrom(nil)
}

// runFrom starts from an already-selected item when the manager needs the
// command response to expose that item immediately. A nil item preserves the
// ordinary selector-driven startup path.
func (p *player) runFrom(item *QueueItem) {
	defer p.disconnect()
	go p.heartbeat()
	for {
		if item == nil {
			item = p.advance(nil, transitionNatural, 0)
			if item == nil {
				p.clearCurrent()
				// Queue exhausted: wait briefly for something to become ready.
				if !p.idleWait() {
					return
				}
				continue
			}
			p.setCurrent(item)
		}
		result := p.playFile(item)
		if result.stop {
			acknowledge(result.ack)
			return
		}
		if result.priority != nil {
			item = result.priority
			p.setPriorityCurrent(item)
			acknowledge(result.ack)
			continue
		}
		item = p.advance(item, result.transition, result.positionMS)
		if item == nil {
			p.clearCurrent()
		} else {
			p.setCurrent(item)
		}
		// next/previous is acknowledged only after the authoritative current
		// item has changed (or the queue was proven empty).
		acknowledge(result.ack)
	}
}

func acknowledge(ack chan struct{}) {
	if ack != nil {
		close(ack)
	}
}

// idleWait blocks up to 30s for something to do when the queue is empty.
// Returns false if we should shut the player down (stop, or timeout).
func (p *player) idleWait() bool {
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case c := <-p.cmd:
			if c.kind == cmdStop {
				acknowledge(c.ack)
				return false
			}
			if c.kind == cmdPlayNow && c.item != nil {
				p.setPriorityCurrent(c.item)
				acknowledge(c.ack)
				return true
			}
			// With no current track there is nothing to pause/skip/restart.  The
			// command still has a deterministic successful acknowledgement and a
			// wake asks the loop to re-check the queue immediately.
			acknowledge(c.ack)
			return true
		case <-timer.C:
			return false
		}
	}
}

// heartbeat asks the manager for a compact progress update once per second
// while a track is actively playing. The audio write loop only records the
// position locally (setPosition); keeping fan-out on a separate goroutine
// avoids network work on the 20ms sample-pacing path. Compatibility policy
// (compact-only or compact plus a legacy full snapshot) belongs to the manager.
func (p *player) heartbeat() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			if p.isPlaying() && p.onHeartbeat != nil {
				p.onHeartbeat()
			}
		}
	}
}

// isPlaying reports whether a track is actively streaming (running, not paused,
// with a current item) — the only state in which the position is advancing.
func (p *player) isPlaying() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.stopped && !p.paused && p.current != nil
}

// pause/resume/skip/stop. Returns true if the player should stop entirely.
func (p *player) playFile(item *QueueItem) playResult {
	releaseLease := func() {}
	if p.acquireLease != nil {
		releaseLease = p.acquireLease(item)
	}
	defer releaseLease()
	f, err := os.Open(item.FilePath)
	if err != nil {
		return playResult{transition: transitionNatural} // skip a vanished file
	}
	defer f.Close()

	ogg, _, err := oggreader.NewOggReader(f)
	if err != nil {
		return playResult{transition: transitionNatural}
	}

	// Keep the existing pause state across previous/next. A paused listener who
	// changes tracks must remain paused until an explicit resume command.
	var elapsed time.Duration
	nextWrite := time.Now()

	for {
		// Drain any pending control commands without blocking.
		select {
		case c := <-p.cmd:
			switch c.kind {
			case cmdStop:
				return playResult{stop: true, positionMS: elapsed.Milliseconds(), ack: c.ack}
			case cmdSkip:
				return playResult{transition: transitionNext, positionMS: elapsed.Milliseconds(), ack: c.ack}
			case cmdPrevious:
				return playResult{transition: transitionPrevious, positionMS: elapsed.Milliseconds(), ack: c.ack}
			case cmdPlayNow:
				if c.item != nil {
					return playResult{priority: c.item, positionMS: elapsed.Milliseconds(), ack: c.ack}
				}
				acknowledge(c.ack)
			case cmdPause:
				p.setPaused(true)
				acknowledge(c.ack)
			case cmdResume:
				p.setPaused(false)
				nextWrite = time.Now() // resync clock; don't burst-catch-up
				acknowledge(c.ack)
			case cmdWake:
				acknowledge(c.ack)
			}
		default:
		}

		if p.isPaused() {
			// Block until a command arrives, then re-evaluate. While paused we
			// write nothing, so RTP simply stops — listeners hear silence.
			c := <-p.cmd
			switch c.kind {
			case cmdStop:
				return playResult{stop: true, positionMS: elapsed.Milliseconds(), ack: c.ack}
			case cmdSkip:
				return playResult{transition: transitionNext, positionMS: elapsed.Milliseconds(), ack: c.ack}
			case cmdPrevious:
				return playResult{transition: transitionPrevious, positionMS: elapsed.Milliseconds(), ack: c.ack}
			case cmdPlayNow:
				if c.item != nil {
					return playResult{priority: c.item, positionMS: elapsed.Milliseconds(), ack: c.ack}
				}
				acknowledge(c.ack)
			case cmdResume:
				p.setPaused(false)
				nextWrite = time.Now()
				acknowledge(c.ack)
			case cmdPause:
				// already paused
				acknowledge(c.ack)
			case cmdWake:
				acknowledge(c.ack)
			}
			continue
		}

		payload, err := ogg.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return playResult{transition: transitionNatural, positionMS: elapsed.Milliseconds()}
			}
			// A non-EOF read error means a corrupt/truncated Opus file. Don't
			// silently vanish it — log so the failure is diagnosable, then
			// advance to the next track.
			log.Printf("musicbox: room %s track %q read error, skipping: %v", p.roomID, item.ID, err)
			return playResult{transition: transitionNatural, positionMS: elapsed.Milliseconds()}
		}
		dur, err := oggreader.ParsePacketDuration(payload)
		if err != nil || dur <= 0 {
			dur = 20 * time.Millisecond
		}

		p.mu.Lock()
		track := p.opusTrack
		p.mu.Unlock()
		if track == nil {
			return playResult{stop: true, positionMS: elapsed.Milliseconds()}
		}
		if err := track.WriteSample(media.Sample{Data: payload, Duration: dur}, nil); err != nil {
			return playResult{transition: transitionNatural, positionMS: elapsed.Milliseconds()}
		}

		elapsed += dur
		p.setPosition(elapsed.Milliseconds())

		// Pace to real time so we don't blast the whole file instantly.
		nextWrite = nextWrite.Add(dur)
		sleep := time.Until(nextWrite)
		if sleep > 0 {
			time.Sleep(sleep)
		}
	}
}

// Control commands (non-blocking; the loop drains them) -----------------------

func (p *player) pause() error    { return p.sendAndWait(cmdPause) }
func (p *player) resume() error   { return p.sendAndWait(cmdResume) }
func (p *player) skip() error     { return p.sendAndWait(cmdSkip) }
func (p *player) previous() error { return p.sendAndWait(cmdPrevious) }
func (p *player) playNow(item *QueueItem) error {
	if item == nil {
		return errors.New("music box priority item is required")
	}
	return p.sendCommandAndWait(command{kind: cmdPlayNow, item: item})
}
func (p *player) stop() error {
	if err := p.sendAndWait(cmdStop); err != nil {
		return err
	}
	select {
	case <-p.done:
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("music box player shutdown timed out")
	}
}
func (p *player) wake() { p.send(command{kind: cmdWake}) }

func (p *player) sendAndWait(kind commandKind) error {
	return p.sendCommandAndWait(command{kind: kind})
}

func (p *player) sendCommandAndWait(c command) error {
	ack := make(chan struct{})
	c.ack = ack
	p.mu.Lock()
	stopped := p.stopped
	p.mu.Unlock()
	if stopped {
		return errors.New("music box player is stopped")
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	// Reliable controls wait for transient wake-command pressure to drain.
	// Wake itself remains best-effort and coalescible through send().
	select {
	case p.cmd <- c:
	case <-p.done:
		return errors.New("music box player is stopped")
	case <-timer.C:
		return errors.New("music box command timed out")
	}
	select {
	case <-ack:
		return nil
	case <-p.done:
		select {
		case <-ack:
			return nil
		default:
		}
		return errors.New("music box player is stopped")
	case <-timer.C:
		return errors.New("music box command timed out")
	}
}

// setPriorityCurrent changes the file being read without tearing down the
// LiveKit participant or published Opus track. Priority play always starts at
// zero and clears a previous pause, including when restarting the same item.
func (p *player) setPriorityCurrent(it *QueueItem) {
	p.mu.Lock()
	p.current = it
	p.positionMS = 0
	p.paused = false
	p.mu.Unlock()
	if p.onPriorityState != nil {
		p.onPriorityState()
	} else if p.onState != nil {
		p.onState()
	}
}

func (p *player) send(c command) bool {
	p.mu.Lock()
	stopped := p.stopped
	p.mu.Unlock()
	if stopped {
		return false
	}
	select {
	case p.cmd <- c:
		return true
	default:
		return false
	}
}

func (p *player) disconnect() {
	p.mu.Lock()
	p.stopped = true
	room := p.room
	track := p.opusTrack
	p.room = nil
	p.opusTrack = nil
	p.current = nil
	p.mu.Unlock()
	// Publish completion only after snapshot() can no longer report the old
	// track. Stop/switch HTTP responses wait on this boundary.
	p.doneOnce.Do(func() { close(p.done) })
	if track != nil {
		_ = track.Close()
	}
	if room != nil {
		room.Disconnect()
	}
	if p.onState != nil {
		p.onState()
	}
}

// state accessors -------------------------------------------------------------

func (p *player) setPaused(v bool) {
	p.mu.Lock()
	changed := p.paused != v
	p.paused = v
	p.mu.Unlock()
	if changed && p.onState != nil {
		p.onState()
	}
}

func (p *player) isPaused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

func (p *player) setCurrent(it *QueueItem) {
	p.mu.Lock()
	p.current = it
	p.positionMS = 0
	p.mu.Unlock()
	if p.onState != nil {
		p.onState()
	}
}

func (p *player) clearCurrent() {
	p.mu.Lock()
	changed := p.current != nil || p.positionMS != 0
	p.current = nil
	p.positionMS = 0
	p.mu.Unlock()
	if changed && p.onState != nil {
		p.onState()
	}
}

func (p *player) setPosition(ms int64) {
	p.mu.Lock()
	p.positionMS = ms
	p.mu.Unlock()
}

// snapshot returns the live playback state for persistence/SSE.
func (p *player) snapshot() (state PlaybackState, currentID string, positionMS int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return StateStopped, "", 0
	}
	if p.current == nil {
		return StateStopped, "", 0
	}
	if p.paused {
		return StatePaused, p.current.ID, p.positionMS
	}
	return StatePlaying, p.current.ID, p.positionMS
}
