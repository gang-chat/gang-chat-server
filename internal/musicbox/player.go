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
	roomID     string
	host       string
	token      string
	opusTrack  *lksdk.LocalTrack

	mu        sync.Mutex
	room      *lksdk.Room
	cmd       chan command
	paused    bool
	stopped   bool
	current   *QueueItem
	positionMS int64

	// advance returns the next item to play after the given one finishes (or
	// the first item if prev is nil). Returns nil when the queue is exhausted.
	advance func(prev *QueueItem) *QueueItem
	// onState is called whenever playback state changes so the manager can
	// persist it and fan out an SSE snapshot.
	onState func()
}

type commandKind int

const (
	cmdPause commandKind = iota
	cmdResume
	cmdSkip
	cmdStop
	cmdWake // re-check the queue when idling; no-op during playback
)

type command struct {
	kind commandKind
}

func newPlayer(roomID, host, token string, advance func(prev *QueueItem) *QueueItem, onState func()) *player {
	return &player{
		roomID:  roomID,
		host:    host,
		token:   token,
		cmd:     make(chan command, 8),
		advance: advance,
		onState: onState,
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
	defer p.disconnect()
	var prev *QueueItem
	for {
		item := p.advance(prev)
		if item == nil {
			// Queue exhausted: wait briefly for new ready items, then exit so
			// the bot doesn't linger. A control command (skip/stop) also wakes us.
			if !p.idleWait() {
				return
			}
			prev = nil
			continue
		}
		p.setCurrent(item)
		stop := p.playFile(item)
		prev = item
		if stop {
			return
		}
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
				return false
			}
			// Any other command: re-check the queue immediately.
			return true
		case <-timer.C:
			return false
		}
	}
}

// playFile streams one Opus file into the track sample-by-sample, honoring
// pause/resume/skip/stop. Returns true if the player should stop entirely.
func (p *player) playFile(item *QueueItem) (stopPlayer bool) {
	f, err := os.Open(item.FilePath)
	if err != nil {
		return false // skip a vanished file, keep going
	}
	defer f.Close()

	ogg, _, err := oggreader.NewOggReader(f)
	if err != nil {
		return false
	}

	p.setState(false) // ensure not-paused at track start unless told otherwise
	var elapsed time.Duration
	nextWrite := time.Now()

	for {
		// Drain any pending control commands without blocking.
		select {
		case c := <-p.cmd:
			switch c.kind {
			case cmdStop:
				return true
			case cmdSkip:
				return false
			case cmdPause:
				p.setPaused(true)
			case cmdResume:
				p.setPaused(false)
				nextWrite = time.Now() // resync clock; don't burst-catch-up
			}
		default:
		}

		if p.isPaused() {
			// Block until a command arrives, then re-evaluate. While paused we
			// write nothing, so RTP simply stops — listeners hear silence.
			c := <-p.cmd
			switch c.kind {
			case cmdStop:
				return true
			case cmdSkip:
				return false
			case cmdResume:
				p.setPaused(false)
				nextWrite = time.Now()
			case cmdPause:
				// already paused
			}
			continue
		}

		payload, err := ogg.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false // track finished, advance to next
			}
			// A non-EOF read error means a corrupt/truncated Opus file. Don't
			// silently vanish it — log so the failure is diagnosable, then
			// advance to the next track.
			log.Printf("musicbox: room %s track %q read error, skipping: %v", p.roomID, item.ID, err)
			return false
		}
		dur, err := oggreader.ParsePacketDuration(payload)
		if err != nil || dur <= 0 {
			dur = 20 * time.Millisecond
		}

		p.mu.Lock()
		track := p.opusTrack
		p.mu.Unlock()
		if track == nil {
			return true
		}
		if err := track.WriteSample(media.Sample{Data: payload, Duration: dur}, nil); err != nil {
			return false
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

func (p *player) pause()  { p.send(command{kind: cmdPause}) }
func (p *player) resume() { p.send(command{kind: cmdResume}) }
func (p *player) skip()   { p.send(command{kind: cmdSkip}) }
func (p *player) stop()   { p.send(command{kind: cmdStop}) }
func (p *player) wake()   { p.send(command{kind: cmdWake}) }

func (p *player) send(c command) {
	p.mu.Lock()
	stopped := p.stopped
	p.mu.Unlock()
	if stopped {
		return
	}
	select {
	case p.cmd <- c:
	default:
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

func (p *player) setState(paused bool) { p.setPaused(paused) }

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
