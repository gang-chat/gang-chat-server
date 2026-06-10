package musicbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/zhuangkaiyi/gang-chat/server/internal/gdmusic"
)

// ErrQueueFull is returned by Enqueue when adding a track would push a room's
// transcoded audio past its byte cap.
var ErrQueueFull = errors.New("music box queue size limit reached")

// ErrUnavailable is returned when the music box can't operate (LiveKit not
// configured). Handlers map it to 503.
var ErrUnavailable = errors.New("music box is not available")

// TokenFunc issues a LiveKit join token for the bot in a room. Provided by the
// caller so token policy (TTL, identity, grants) stays in one place.
type TokenFunc func(roomID, identity string) (token string, err error)

// Config wires the manager to its dependencies.
type Config struct {
	Dir              string // base dir for transcoded files
	MaxBytesPerRoom  int64
	FFmpegPath       string
	OpusBitrate      string
	TranscodeWorkers int
	Source           string // default GD source
	SourceBitrate    string // GD download quality
	LiveKitHost      string
	Enabled          bool // false when LiveKit isn't configured
}

// Manager owns all room music boxes: the queue store, the transcode pool, the
// GD API client, and one player per actively-playing room.
type Manager struct {
	cfg     Config
	store   *store
	tc      *transcoder
	gd      *gdmusic.Client
	tokenFn TokenFunc

	// onRoomChanged is invoked (room id) whenever a room's music box state or
	// queue changes, so the chat layer can fan out an SSE snapshot.
	onRoomChanged func(roomID string)

	mu      sync.Mutex
	players map[string]*player
}

// NewManager builds a Manager. If cfg.Enabled is false every operation returns
// ErrUnavailable, so callers don't need nil checks.
func NewManager(db *sql.DB, cfg Config, tokenFn TokenFunc, onRoomChanged func(string)) *Manager {
	gd := gdmusic.New(
		gdmusic.WithDefaultSource(cfg.Source),
		gdmusic.WithDefaultBitrate(cfg.SourceBitrate),
	)
	return &Manager{
		cfg:           cfg,
		store:         &store{db: db},
		tc:            newTranscoder(cfg.FFmpegPath, cfg.OpusBitrate, cfg.TranscodeWorkers),
		gd:            gd,
		tokenFn:       tokenFn,
		onRoomChanged: onRoomChanged,
		players:       map[string]*player{},
	}
}

// GD exposes the underlying API client for search/lyric passthrough handlers.
func (m *Manager) GD() *gdmusic.Client { return m.gd }

// SetOnRoomChanged installs the change callback after construction. The chat
// layer uses this to fan out an SSE snapshot, but it owns the Handler that
// builds the snapshot, so it can't be supplied at NewManager time. Set once at
// startup before any playback begins.
func (m *Manager) SetOnRoomChanged(fn func(roomID string)) {
	m.onRoomChanged = fn
}

// Enabled reports whether the music box can broadcast.
func (m *Manager) Enabled() bool { return m != nil && m.cfg.Enabled }

func (m *Manager) notify(roomID string) {
	if m.onRoomChanged != nil {
		m.onRoomChanged(roomID)
	}
}

// EnqueueParams describes a track to add to a room's queue.
type EnqueueParams struct {
	RoomID        string
	Source        string
	TrackID       string
	Title         string
	Artist        string
	Album         string
	PicID         string
	LyricID       string
	DurationMS    int64
	AddedByUserID string
}

// Enqueue adds a track and kicks off its download+transcode immediately. It
// rejects the add if the room is already at (or the new track's rough size
// would exceed) the byte cap. Because the real size isn't known until
// transcoding finishes, we reserve a conservative estimate up front and
// reconcile on completion.
func (m *Manager) Enqueue(ctx context.Context, p EnqueueParams) (*QueueItem, error) {
	if !m.Enabled() {
		return nil, ErrUnavailable
	}
	// Reject when the room is already over the cap (counts ready+downloading).
	used, err := m.store.roomReadyBytes(p.RoomID)
	if err != nil {
		return nil, err
	}
	if used >= m.cfg.MaxBytesPerRoom {
		return nil, ErrQueueFull
	}

	source := p.Source
	if source == "" {
		source = m.cfg.Source
	}
	sortOrder, err := m.store.nextSortOrder(p.RoomID)
	if err != nil {
		return nil, err
	}
	item, err := m.store.insertItem(QueueItem{
		ID:            "mbx_" + randomID(),
		RoomID:        p.RoomID,
		Source:        source,
		TrackID:       p.TrackID,
		Title:         p.Title,
		Artist:        p.Artist,
		Album:         p.Album,
		PicID:         p.PicID,
		LyricID:       p.LyricID,
		DurationMS:    p.DurationMS,
		Status:        StatusPending,
		AddedByUserID: p.AddedByUserID,
		SortOrder:     sortOrder,
	})
	if err != nil {
		return nil, err
	}
	m.notify(p.RoomID)
	go m.process(item.ID)
	return item, nil
}

// process resolves the track URL, transcodes it, updates the row, and nudges
// the room's player to (re)start if it was idle/exhausted.
func (m *Manager) process(itemID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	item, err := m.store.getItem(itemID)
	if err != nil {
		return
	}
	_ = m.store.setStatus(itemID, StatusDownloading)
	m.notify(item.RoomID)

	resolved, err := m.gd.TrackURL(ctx, item.Source, item.TrackID, m.cfg.SourceBitrate)
	if err != nil {
		_ = m.store.markFailed(itemID, "resolve url: "+err.Error())
		m.notify(item.RoomID)
		return
	}

	roomDir := filepath.Join(m.cfg.Dir, sanitize(item.RoomID))
	if err := os.MkdirAll(roomDir, 0o755); err != nil {
		_ = m.store.markFailed(itemID, "prepare dir: "+err.Error())
		m.notify(item.RoomID)
		return
	}
	dst := filepath.Join(roomDir, itemID+".ogg")

	res, err := m.tc.transcode(ctx, resolved.URL, dst)
	if err != nil {
		_ = m.store.markFailed(itemID, err.Error())
		m.notify(item.RoomID)
		return
	}

	// Enforce the cap after the real size is known: if this file pushed the
	// room over, drop it rather than letting disk grow unbounded.
	used, _ := m.store.roomReadyBytes(item.RoomID)
	if used+res.SizeBytes > m.cfg.MaxBytesPerRoom {
		_ = os.Remove(dst)
		_ = m.store.markFailed(itemID, ErrQueueFull.Error())
		m.notify(item.RoomID)
		return
	}

	if err := m.store.markReady(itemID, dst, res.SizeBytes, res.DurationMS); err != nil {
		_ = os.Remove(dst)
		return
	}
	m.notify(item.RoomID)
	// A track is ready: make sure the room is playing it (no-op if already).
	m.ensurePlaying(item.RoomID)
}

// Control applies a playback action to a room. Valid actions: play, pause,
// resume, skip, stop.
func (m *Manager) Control(roomID, action string) error {
	if !m.Enabled() {
		return ErrUnavailable
	}
	switch action {
	case "play", "resume":
		return m.play(roomID, action == "resume")
	case "pause":
		if pl := m.getPlayer(roomID); pl != nil {
			pl.pause()
		}
		return nil
	case "skip", "next":
		if pl := m.getPlayer(roomID); pl != nil {
			pl.skip()
		} else {
			return m.play(roomID, false)
		}
		return nil
	case "stop":
		m.stopRoom(roomID)
		return nil
	default:
		return fmt.Errorf("unknown action %q", action)
	}
}

func (m *Manager) play(roomID string, resumeOnly bool) error {
	if pl := m.getPlayer(roomID); pl != nil {
		pl.resume()
		return nil
	}
	if resumeOnly {
		// Nothing playing and caller only asked to resume: start fresh.
	}
	return m.ensurePlaying(roomID)
}

// ensurePlaying starts a player for the room if one isn't running and there's
// at least one ready track. Safe to call repeatedly.
func (m *Manager) ensurePlaying(roomID string) error {
	m.mu.Lock()
	if pl, ok := m.players[roomID]; ok {
		m.mu.Unlock()
		// Already running. If it's idling on an empty queue, nudge it so it
		// re-checks and picks up the newly-ready track instead of waiting out
		// its idle timeout.
		pl.wake()
		return nil
	}
	// Reserve the slot under lock to avoid two concurrent starts.
	first, err := m.store.firstPlayable(roomID, -1)
	if err != nil || first == nil {
		m.mu.Unlock()
		return err
	}
	token, err := m.tokenFn(roomID, botIdentity)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	pl := newPlayer(roomID, m.cfg.LiveKitHost, token,
		func(prev *QueueItem) *QueueItem { return m.nextItem(roomID, prev) },
		func() { m.persistAndNotify(roomID) },
	)
	m.players[roomID] = pl
	m.mu.Unlock()

	if err := pl.connect(); err != nil {
		m.mu.Lock()
		delete(m.players, roomID)
		m.mu.Unlock()
		return err
	}
	go func() {
		pl.run()
		m.mu.Lock()
		if m.players[roomID] == pl {
			delete(m.players, roomID)
		}
		m.mu.Unlock()
		m.persistAndNotify(roomID)
	}()
	return nil
}

// nextItem is the player's advance callback: pick the next ready track after
// prev (or the first ready track when prev is nil).
func (m *Manager) nextItem(roomID string, prev *QueueItem) *QueueItem {
	after := int64(-1)
	if prev != nil {
		after = prev.SortOrder
	}
	it, err := m.store.firstPlayable(roomID, after)
	if err != nil {
		return nil
	}
	return it
}

func (m *Manager) stopRoom(roomID string) {
	pl := m.getPlayer(roomID)
	if pl != nil {
		pl.stop()
	}
	st, _ := m.store.getState(roomID)
	if st != nil {
		st.State = StateStopped
		st.CurrentItemID = ""
		st.PositionMS = 0
		_ = m.store.saveState(*st)
	}
	m.notify(roomID)
}

func (m *Manager) getPlayer(roomID string) *player {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.players[roomID]
}

// persistAndNotify writes the live player snapshot to the DB and fans out.
func (m *Manager) persistAndNotify(roomID string) {
	pl := m.getPlayer(roomID)
	st, _ := m.store.ensureState(roomID)
	if st == nil {
		st = &RoomState{RoomID: roomID, Volume: 100}
	}
	if pl != nil {
		state, currentID, pos := pl.snapshot()
		st.State = state
		st.CurrentItemID = currentID
		st.PositionMS = pos
	} else {
		st.State = StateStopped
		st.PositionMS = 0
	}
	_ = m.store.saveState(*st)
	m.notify(roomID)
}

// RemoveItem deletes a queue item, removing its file. If it's the track
// currently playing, the player skips to the next.
func (m *Manager) RemoveItem(roomID, itemID string) error {
	if !m.Enabled() {
		return ErrUnavailable
	}
	pl := m.getPlayer(roomID)
	playingCurrent := false
	if pl != nil {
		_, currentID, _ := pl.snapshot()
		playingCurrent = currentID == itemID
	}
	item, err := m.store.deleteItem(itemID)
	if err != nil {
		return err
	}
	if item != nil && item.FilePath != "" {
		_ = os.Remove(item.FilePath)
	}
	if playingCurrent && pl != nil {
		pl.skip()
	}
	m.notify(roomID)
	return nil
}

// State returns the persisted room state and the current queue.
func (m *Manager) State(roomID string) (*RoomState, []*QueueItem, error) {
	st, err := m.store.getState(roomID)
	if err != nil {
		return nil, nil, err
	}
	// Prefer the live player's view when one is running.
	if pl := m.getPlayer(roomID); pl != nil {
		state, currentID, pos := pl.snapshot()
		st.State = state
		st.CurrentItemID = currentID
		st.PositionMS = pos
	}
	items, err := m.store.listQueue(roomID)
	if err != nil {
		return nil, nil, err
	}
	return st, items, nil
}

// RoomUsage returns bytes used and the cap, for surfacing to clients.
func (m *Manager) RoomUsage(roomID string) (used, cap int64) {
	used, _ = m.store.roomReadyBytes(roomID)
	return used, m.cfg.MaxBytesPerRoom
}

func randomID() string {
	return uuid.NewString()
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}
