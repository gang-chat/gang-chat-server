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
	"github.com/zhuangkaiyi/gang-chat/server/internal/meloyou"
)

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
	router  *meloyou.Router
	tokenFn TokenFunc

	// onRoomChanged is invoked (room id) whenever a room's music box state or
	// queue changes, so the chat layer can fan out an SSE snapshot.
	onRoomChanged func(roomID string)

	mu      sync.Mutex
	players map[string]*player

	// pumpMu serializes the download scheduler (pumpRoom) so two concurrent
	// triggers can't both start a download for the same room.
	pumpMu sync.Mutex
}

// NewManager builds a Manager. If cfg.Enabled is false every operation returns
// ErrUnavailable, so callers don't need nil checks.
func NewManager(db *sql.DB, cfg Config, tokenFn TokenFunc, onRoomChanged func(string)) *Manager {
	gd := gdmusic.New(
		gdmusic.WithDefaultSource(cfg.Source),
		gdmusic.WithDefaultBitrate(cfg.SourceBitrate),
	)
	m := &Manager{
		cfg:           cfg,
		store:         &store{db: db},
		tc:            newTranscoder(cfg.FFmpegPath, cfg.OpusBitrate, cfg.TranscodeWorkers),
		router:        meloyou.New(gd),
		tokenFn:       tokenFn,
		onRoomChanged: onRoomChanged,
		players:       map[string]*player{},
	}
	// A restart wipes the music box clean: clear every room's queue and state
	// in the DB and delete all transcoded files on disk. Nothing here is worth
	// preserving across restarts — playback can't resume mid-track anyway, and
	// starting empty guarantees no stale (e.g. pre-FEC) cached files linger.
	if cfg.Enabled {
		m.resetOnStartup()
	}
	return m
}

// resetOnStartup clears all queued tracks and playback state, then removes the
// on-disk transcode directory. Best-effort: a failure to clean disk is logged
// implicitly by leaving files behind, but never blocks startup.
func (m *Manager) resetOnStartup() {
	_ = m.store.clearAllQueues()
	if m.cfg.Dir != "" {
		// Remove the whole tree (per-room subdirs of .ogg files), then recreate
		// the base dir so the first download has somewhere to write.
		_ = os.RemoveAll(m.cfg.Dir)
		_ = os.MkdirAll(m.cfg.Dir, 0o755)
	}
}

// Router exposes the underlying source router for the search passthrough
// handler.
func (m *Manager) Router() *meloyou.Router { return m.router }

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
	DurationMS    int64
	AddedByUserID string
}

// Enqueue appends a track to a room's queue. The queue itself is unbounded:
// the byte cap (MaxBytesPerRoom) only governs how many tracks are downloaded
// and held on disk at once, not how many can be queued. A newly enqueued track
// starts as pending and is picked up by pumpRoom once there's room on disk.
func (m *Manager) Enqueue(ctx context.Context, p EnqueueParams) (*QueueItem, error) {
	if !m.Enabled() {
		return nil, ErrUnavailable
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
		DurationMS:    p.DurationMS,
		Status:        StatusPending,
		AddedByUserID: p.AddedByUserID,
		SortOrder:     sortOrder,
	})
	if err != nil {
		return nil, err
	}
	m.notify(p.RoomID)
	// Try to start downloading immediately; pumpRoom is a no-op if the room is
	// already at its disk cap, in which case the track waits as pending.
	go m.pumpRoom(p.RoomID)
	return item, nil
}

// pumpRoom starts downloading the next pending track(s) for a room while there
// is room under the byte cap. It downloads at most one track at a time per room
// (a track in flight reserves no real bytes until it finishes, so we serialize
// to keep the on-disk total predictable). An empty room (zero bytes used) is
// always allowed to start one track, so a single track larger than the cap can
// still play rather than being stuck forever.
func (m *Manager) pumpRoom(roomID string) {
	if !m.Enabled() {
		return
	}
	m.pumpMu.Lock()
	defer m.pumpMu.Unlock()

	// Only one download in flight per room.
	inflight, err := m.store.countDownloading(roomID)
	if err != nil || inflight > 0 {
		return
	}
	used, err := m.store.roomReadyBytes(roomID)
	if err != nil {
		return
	}
	// Stop pumping once at/over the cap, but always allow the first track in an
	// empty room so an oversized single track isn't wedged.
	if used >= m.cfg.MaxBytesPerRoom && used > 0 {
		return
	}
	next, err := m.store.firstPending(roomID)
	if err != nil || next == nil {
		return
	}
	// Reserve the slot by flipping to downloading under the pump lock, so a
	// concurrent pumpRoom sees inflight > 0 and backs off.
	if err := m.store.setStatus(next.ID, StatusDownloading); err != nil {
		return
	}
	m.notify(roomID)
	go m.process(next.ID)
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
	// Status is already 'downloading' (set by pumpRoom under its lock).

	resolved, err := m.router.TrackURL(ctx, item.Source, item.TrackID, m.cfg.SourceBitrate)
	if err != nil {
		_ = m.store.markFailed(itemID, "resolve url: "+err.Error())
		m.notify(item.RoomID)
		go m.pumpRoom(item.RoomID)
		return
	}

	roomDir := filepath.Join(m.cfg.Dir, sanitize(item.RoomID))
	if err := os.MkdirAll(roomDir, 0o755); err != nil {
		_ = m.store.markFailed(itemID, "prepare dir: "+err.Error())
		m.notify(item.RoomID)
		go m.pumpRoom(item.RoomID)
		return
	}
	dst := filepath.Join(roomDir, itemID+".ogg")

	res, err := m.tc.transcode(ctx, item.Source, resolved.URL, dst)
	if err != nil {
		_ = m.store.markFailed(itemID, err.Error())
		m.notify(item.RoomID)
		go m.pumpRoom(item.RoomID)
		return
	}

	// No post-transcode cap check: pumpRoom already gated this download on the
	// cap before starting it, and we allow one in-flight track to push the room
	// up to ~cap + one track. The cap simply prevents the *next* download from
	// starting until space frees up (a played track is removed from the queue).
	if err := m.store.markReady(itemID, dst, res.SizeBytes, res.DurationMS); err != nil {
		_ = os.Remove(dst)
		go m.pumpRoom(item.RoomID)
		return
	}
	m.notify(item.RoomID)
	// A track is ready: make sure the room is playing it (no-op if already).
	m.ensurePlaying(item.RoomID)
	// Try the next pending track (no-op if now at the cap).
	go m.pumpRoom(item.RoomID)
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

// nextItem is the player's advance callback. The just-finished track (prev) is
// consumed: removed from the queue and its file deleted, which frees disk space
// so the next pending track can start downloading. It then returns the next
// ready track, or nil when none is ready yet (the player idles and is woken
// when a download completes).
func (m *Manager) nextItem(roomID string, prev *QueueItem) *QueueItem {
	after := int64(-1)
	if prev != nil {
		after = prev.SortOrder
		// Consume the finished track and reclaim its space, then let the
		// scheduler pull in whatever was waiting on that space.
		if removed, err := m.store.deleteItem(prev.ID); err == nil && removed != nil && removed.FilePath != "" {
			_ = os.Remove(removed.FilePath)
		}
		m.notify(roomID)
		go m.pumpRoom(roomID)
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
	// Removing a ready track frees disk space; let a pending track download.
	go m.pumpRoom(roomID)
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
