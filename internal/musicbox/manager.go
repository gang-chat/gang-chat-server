package musicbox

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/zhuangkaiyi/gang-chat/server/internal/gdmusic"
	"github.com/zhuangkaiyi/gang-chat/server/internal/qqmusic"
)

// SourceTencent routes search and URL resolution to the self-hosted QQ音乐
// service instead of the GD API. It matches the source value the client sends
// for QQ音乐 tracks.
const SourceTencent = "tencent"

const defaultGDSource = "netease"

var (
	// ErrUnavailable is returned when the music box can't operate (LiveKit not
	// configured). Handlers map it to 503.
	ErrUnavailable            = errors.New("music box is not available")
	ErrRevisionConflict       = errors.New("music box revision conflict")
	ErrQueueItemNotFound      = errors.New("music box queue item not found")
	ErrQueueItemNotReady      = errors.New("music box queue item not ready")
	ErrQueueItemAlreadyExists = errors.New("music box queue item already exists")
)

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
	DownloadBitrate  string // GD download quality
	LiveKitHost      string
	Enabled          bool // false when LiveKit isn't configured

	// QQ is the optional self-hosted QQ音乐 client. nil disables the tencent
	// source; search/resolve for it then return an error.
	QQ *qqmusic.Client
}

// Manager owns all room music boxes: the queue store, the transcode pool, the
// GD API client, and one player per actively-playing room.
type Manager struct {
	cfg     Config
	store   *store
	tc      *transcoder
	gd      *gdmusic.Client
	qq      *qqmusic.Client // nil when QQ音乐 integration is disabled
	search  *searchCoordinator
	tokenFn TokenFunc

	// onRoomChanged is invoked (room id) whenever a room's music box state or
	// queue changes, so the chat layer can fan out an SSE snapshot.
	onRoomChanged func(roomID string)

	mu           sync.Mutex
	players      map[string]*player
	controlLocks sync.Map // map[string]*sync.Mutex
	persistMu    sync.Mutex
	seenCommands map[string]map[string]int64
	playCursors  map[string]playCursor

	// pumpMu serializes the download scheduler (pumpRoom) so two concurrent
	// triggers can't both start a download for the same room.
	pumpMu sync.Mutex

	// previewCleanupMu serializes best-effort LRU cleanup of the shared
	// authenticated preview cache. Per-track preparation itself is serialized
	// through controlLocks so concurrent clients do not transcode the same track
	// more than once.
	previewCleanupMu sync.Mutex
}

const previewCacheMaxBytes int64 = 512 << 20

// NewManager builds a Manager. If cfg.Enabled is false every operation returns
// ErrUnavailable, so callers don't need nil checks.
func NewManager(db *sql.DB, cfg Config, tokenFn TokenFunc, onRoomChanged func(string)) *Manager {
	gd := gdmusic.New(
		gdmusic.WithDefaultBitrate(cfg.DownloadBitrate),
	)
	m := &Manager{
		cfg:           cfg,
		store:         &store{db: db},
		tc:            newTranscoder(cfg.FFmpegPath, cfg.OpusBitrate, cfg.TranscodeWorkers),
		gd:            gd,
		qq:            cfg.QQ,
		tokenFn:       tokenFn,
		onRoomChanged: onRoomChanged,
		players:       map[string]*player{},
		seenCommands:  map[string]map[string]int64{},
		playCursors:   map[string]playCursor{},
	}
	m.search = newSearchCoordinator(m.searchUpstream)
	// A restart preserves queues and prepared media, but resets every room to a
	// silent stopped state. Playback resumes only after an explicit command.
	if cfg.Enabled {
		if err := m.store.ensureNextGenerationSchema(); err != nil {
			log.Printf("musicbox: next-generation schema unavailable: %v", err)
			m.cfg.Enabled = false
		} else {
			m.resetOnStartup()
		}
	}
	return m
}

type playCursor struct {
	scope      QueueScope
	snapshotID string
	afterSort  int64
}

// resetOnStartup preserves queue/cache data and only restores a safe silent
// playback state.
func (m *Manager) resetOnStartup() {
	_ = m.store.resetPlaybackOnStartup()
	if m.cfg.Dir != "" {
		_ = os.MkdirAll(m.cfg.Dir, 0o755)
	}
}

// BackfillActivePlaylistCreatedAt upgrades saved sources activated by an older
// server after the playlist schema is available. Deleted source playlists stay
// unknown, which is preferable to inventing a timestamp.
func (m *Manager) BackfillActivePlaylistCreatedAt() error {
	if !m.Enabled() {
		return nil
	}
	return m.store.backfillActivePlaylistCreatedAt()
}

// GD exposes the underlying GD API client (used for album art lookups).
func (m *Manager) GD() *gdmusic.Client { return m.gd }

// QQEnabled reports whether the QQ音乐 (tencent) source is available.
func (m *Manager) QQEnabled() bool { return m.qq != nil }

// SearchTrack is one normalized search hit across sources.
type SearchTrack struct {
	TrackID string
	Name    string
	Artists []string
	Source  string
}

// Search routes a keyword search to the right backend by source. The tencent
// source uses the self-hosted QQ音乐 service; every other source (or empty,
// meaning the GD default) uses the GD API. Results are normalized so callers
// don't branch on source.
func (m *Manager) Search(ctx context.Context, source, keyword string, count, page int) ([]SearchTrack, error) {
	if m.search == nil {
		return m.searchUpstream(ctx, source, keyword, count, page)
	}
	return m.search.Search(ctx, source, keyword, count, page)
}

func (m *Manager) searchUpstream(ctx context.Context, source, keyword string, count, page int) ([]SearchTrack, error) {
	if source == SourceTencent {
		if m.qq == nil {
			return nil, fmt.Errorf("musicbox: QQ音乐 source is not configured")
		}
		hits, err := m.qq.Search(ctx, keyword, count, page)
		if err != nil {
			return nil, err
		}
		out := make([]SearchTrack, 0, len(hits))
		for _, h := range hits {
			out = append(out, SearchTrack{TrackID: h.ID, Name: h.Name, Artists: h.Artists, Source: SourceTencent})
		}
		return out, nil
	}
	hits, err := m.gd.Search(ctx, source, keyword, count, page)
	if err != nil {
		return nil, err
	}
	out := make([]SearchTrack, 0, len(hits))
	for _, h := range hits {
		out = append(out, SearchTrack{TrackID: h.ID, Name: h.Name, Artists: h.Artists, Source: h.Source})
	}
	return out, nil
}

// resolveURL resolves a playable source URL for a queue item by source. The
// tencent source resolves a fresh OGG_192 link via the QQ音乐 service at this
// moment (vkey is short-lived, so we resolve just before transcoding); other
// sources use the GD API.
func (m *Manager) resolveURL(ctx context.Context, item *QueueItem) (string, error) {
	if item.Source == SourceTencent {
		if m.qq == nil {
			return "", fmt.Errorf("musicbox: QQ音乐 source is not configured")
		}
		return m.qq.TrackURL(ctx, item.TrackID)
	}
	resolved, err := m.gd.TrackURL(ctx, item.Source, item.TrackID, m.cfg.DownloadBitrate)
	if err != nil {
		return "", err
	}
	return resolved.URL, nil
}

// PreparePreview resolves and transcodes a track into the shared preview
// cache without adding it to a room queue or touching room playback state.
// The returned M4A/AAC file is served only through the authenticated preview
// endpoint and is used for an explicitly initiated local client preview.
func (m *Manager) PreparePreview(ctx context.Context, source, trackID string) (string, error) {
	if m == nil || m.tc == nil || strings.TrimSpace(m.cfg.Dir) == "" {
		return "", ErrUnavailable
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = defaultGDSource
	}
	trackID = strings.TrimSpace(trackID)
	if trackID == "" {
		return "", errors.New("music preview track id is required")
	}

	key := previewCacheKey(source, trackID)
	lock := m.controlLock("preview:" + key)
	lock.Lock()
	defer lock.Unlock()

	dir := filepath.Join(m.cfg.Dir, "previews")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("prepare preview dir: %w", err)
	}
	dst := filepath.Join(dir, key+".m4a")
	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		now := time.Now()
		_ = os.Chtimes(dst, now, now)
		m.cleanupPreviewCache(dst)
		return dst, nil
	}

	item := &QueueItem{Source: source, TrackID: trackID}
	resolvedURL, err := m.resolveURL(ctx, item)
	if err != nil {
		return "", fmt.Errorf("resolve preview url: %w", err)
	}
	tmp := dst + ".tmp-" + randomID()
	if _, err := m.tc.transcodePreview(ctx, source, resolvedURL, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("publish preview cache: %w", err)
	}
	m.cleanupPreviewCache(dst)
	return dst, nil
}

func previewCacheKey(source, trackID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(source) + "\x00" + strings.TrimSpace(trackID)))
	return fmt.Sprintf("%x", sum[:16])
}

func (m *Manager) cleanupPreviewCache(keepPath string) {
	m.previewCleanupMu.Lock()
	defer m.previewCleanupMu.Unlock()

	dir := filepath.Join(m.cfg.Dir, "previews")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type cachedPreview struct {
		path    string
		size    int64
		modTime time.Time
	}
	files := make([]cachedPreview, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".ogg") || strings.HasSuffix(entry.Name(), ".mp3") {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".m4a") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Size() <= 0 {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		files = append(files, cachedPreview{path: path, size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
	}
	if total <= previewCacheMaxBytes {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for _, file := range files {
		if total <= previewCacheMaxBytes {
			break
		}
		if file.path == keepPath {
			continue
		}
		if err := os.Remove(file.path); err == nil {
			total -= file.size
		}
	}
}

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

func (m *Manager) controlLock(roomID string) *sync.Mutex {
	value, _ := m.controlLocks.LoadOrStore(roomID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func playbackScope(st *RoomState) (QueueScope, string) {
	if st != nil && st.ActiveSourceType != ActiveSourceTemporary && st.ActiveSnapshotID != "" {
		return QueueScopeSavedPlaylistSnapshot, st.ActiveSnapshotID
	}
	return QueueScopeTemporary, ""
}

func (m *Manager) cursorAfter(
	roomID string,
	scope QueueScope,
	snapshotID string,
) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	cursor, ok := m.playCursors[roomID]
	if !ok || cursor.scope != scope || cursor.snapshotID != snapshotID {
		return -1
	}
	return cursor.afterSort
}

func (m *Manager) setCursor(
	roomID string,
	scope QueueScope,
	snapshotID string,
	afterSort int64,
) {
	m.mu.Lock()
	m.playCursors[roomID] = playCursor{
		scope: scope, snapshotID: snapshotID, afterSort: afterSort,
	}
	m.mu.Unlock()
}

func (m *Manager) bumpRevision(roomID string) {
	m.persistMu.Lock()
	st, err := m.store.ensureState(roomID)
	if err == nil && st != nil {
		st.Revision++
		_ = m.store.saveState(*st)
	}
	m.persistMu.Unlock()
	m.notify(roomID)
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
	lock := m.controlLock(p.RoomID)
	lock.Lock()
	defer lock.Unlock()

	source := p.Source
	if source == "" {
		source = defaultGDSource
	}
	temporaryQueue, err := m.store.listScopedQueue(
		p.RoomID,
		QueueScopeTemporary,
		"",
	)
	if err != nil {
		return nil, err
	}
	for _, existing := range temporaryQueue {
		if existing.Source == source && existing.TrackID == p.TrackID {
			return nil, ErrQueueItemAlreadyExists
		}
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
	m.bumpRevision(p.RoomID)
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
	st, _ := m.store.getState(roomID)
	scope, snapshotID := playbackScope(st)
	next, err := m.store.firstPendingInScope(roomID, scope, snapshotID)
	if err != nil || next == nil {
		return
	}
	// Reserve the slot by flipping to downloading under the pump lock, so a
	// concurrent pumpRoom sees inflight > 0 and backs off.
	if err := m.store.setStatus(next.ID, StatusDownloading); err != nil {
		return
	}
	m.bumpRevision(roomID)
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

	resolvedURL, err := m.resolveURL(ctx, item)
	if err != nil {
		_ = m.store.markFailed(itemID, "resolve url: "+err.Error())
		m.bumpRevision(item.RoomID)
		go m.pumpRoom(item.RoomID)
		return
	}

	roomDir := filepath.Join(m.cfg.Dir, sanitize(item.RoomID))
	if err := os.MkdirAll(roomDir, 0o755); err != nil {
		_ = m.store.markFailed(itemID, "prepare dir: "+err.Error())
		m.bumpRevision(item.RoomID)
		go m.pumpRoom(item.RoomID)
		return
	}
	dst := filepath.Join(roomDir, itemID+".ogg")

	res, err := m.tc.transcode(ctx, item.Source, resolvedURL, dst)
	if err != nil {
		_ = m.store.markFailed(itemID, err.Error())
		m.bumpRevision(item.RoomID)
		go m.pumpRoom(item.RoomID)
		return
	}

	// No post-transcode cap check: pumpRoom already gated this download on the
	// cap before starting it, and we allow one in-flight track to push the room
	// up to ~cap + one track. The cap simply prevents the *next* download from
	// starting until space frees up (for example, an operator removes a stored
	// queue item or a playlist snapshot is replaced).
	if err := m.store.markReady(itemID, dst, res.SizeBytes, res.DurationMS); err != nil {
		_ = os.Remove(dst)
		go m.pumpRoom(item.RoomID)
		return
	}
	m.bumpRevision(item.RoomID)
	// A track is ready: make sure the room is playing it (no-op if already).
	m.ensurePlaying(item.RoomID)
	// Try the next pending track (no-op if now at the cap).
	go m.pumpRoom(item.RoomID)
}

// Control applies a playback action to a room. Valid actions: play, pause,
// resume, skip, stop.
func (m *Manager) Control(roomID, action string) error {
	return m.ApplyControl(roomID, action, "", "", nil)
}

// ApplyControl serializes a command, optionally rejects an obsolete client
// revision, and remembers command IDs long enough to make HTTP retries
// idempotent.
func (m *Manager) ApplyControl(
	roomID, action, mode, commandID string,
	expectedRevision *int64,
) error {
	return m.ApplyItemControl(
		roomID,
		action,
		"",
		mode,
		commandID,
		expectedRevision,
	)
}

// ApplyItemControl extends ApplyControl for commands that target a specific
// queue item. Keeping the target inside the same serialized, revision-checked
// command path preserves idempotency across client retries and concurrent
// controllers.
func (m *Manager) ApplyItemControl(
	roomID, action, itemID, mode, commandID string,
	expectedRevision *int64,
) error {
	if !m.Enabled() {
		return ErrUnavailable
	}
	lock := m.controlLock(roomID)
	lock.Lock()
	defer lock.Unlock()

	if commandID != "" {
		m.mu.Lock()
		_, seen := m.seenCommands[roomID][commandID]
		m.mu.Unlock()
		if seen {
			return nil
		}
	}
	if expectedRevision != nil {
		st, err := m.store.getState(roomID)
		if err != nil {
			return err
		}
		if st.Revision != *expectedRevision {
			return ErrRevisionConflict
		}
	}

	var err error
	switch action {
	case "play", "resume":
		err = m.play(roomID, action == "resume")
	case "pause":
		if pl := m.getPlayer(roomID); pl != nil {
			err = pl.pause()
		}
	case "skip", "next":
		if pl := m.getPlayer(roomID); pl != nil {
			err = pl.skip()
		} else {
			err = m.play(roomID, false)
		}
	case "previous":
		if pl := m.getPlayer(roomID); pl != nil {
			err = pl.previous()
		} else {
			err = m.play(roomID, false)
		}
	case "play_now":
		err = m.playNow(roomID, itemID)
	case "clear_temporary_playlist":
		err = m.clearTemporaryQueue(roomID)
	case "set_mode":
		err = m.setPlaybackMode(roomID, mode)
	case "stop":
		err = m.stopRoom(roomID)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		return err
	}
	if commandID != "" {
		m.mu.Lock()
		revisions := m.seenCommands[roomID]
		if revisions == nil {
			revisions = map[string]int64{}
			m.seenCommands[roomID] = revisions
		}
		st, _ := m.store.getState(roomID)
		if st != nil {
			revisions[commandID] = st.Revision
		}
		if len(revisions) > 256 {
			for key := range revisions {
				delete(revisions, key)
				if len(revisions) <= 128 {
					break
				}
			}
		}
		m.mu.Unlock()
	}
	return nil
}

// clearTemporaryQueue removes the room request queue while preserving saved
// playlist snapshots. When that queue is active, playback is stopped first so
// no deleted item remains audible or visible as the current track.
// ApplyItemControl holds the room control lock while this runs.
func (m *Manager) clearTemporaryQueue(roomID string) error {
	st, err := m.store.getState(roomID)
	if err != nil {
		return err
	}
	activeTemporary := st == nil || st.ActiveSourceType == ActiveSourceTemporary
	if activeTemporary {
		if pl := m.getPlayer(roomID); pl != nil {
			if err := pl.stop(); err != nil {
				return err
			}
		}
	}

	items, err := m.store.deleteTemporaryQueue(roomID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.FilePath != "" {
			_ = os.Remove(item.FilePath)
		}
	}

	m.persistMu.Lock()
	st, err = m.store.ensureState(roomID)
	if err == nil && st != nil {
		if activeTemporary {
			st.State = StateStopped
			st.CurrentItemID = ""
			st.PositionMS = 0
		}
		st.Revision++
		err = m.store.saveState(*st)
	}
	m.persistMu.Unlock()
	if err != nil {
		return err
	}
	m.notify(roomID)
	return nil
}

// playNow atomically replaces the current item with another ready item from
// the active queue. While the request queue is actively playing, the selected
// row moves directly after the interrupted row before playback switches. Saved
// playlist snapshots keep their persisted order.
// ApplyItemControl already holds the room control lock while this runs.
func (m *Manager) playNow(roomID, itemID string) error {
	st, err := m.store.getState(roomID)
	if err != nil {
		return err
	}
	item, err := m.store.getRoomItem(roomID, itemID)
	if err != nil {
		return err
	}
	if item == nil {
		return ErrQueueItemNotFound
	}
	scope, snapshotID := playbackScope(st)
	if item.QueueScope != scope ||
		(scope == QueueScopeSavedPlaylistSnapshot && item.SnapshotID != snapshotID) {
		return ErrQueueItemNotFound
	}
	if item.Status != StatusReady {
		return ErrQueueItemNotReady
	}

	activePlayer := m.getPlayer(roomID)
	playbackState := st.State
	currentItemID := st.CurrentItemID
	if activePlayer != nil {
		playbackState, currentItemID, _ = activePlayer.snapshot()
	}
	var originalOrder []string
	reordered := false
	if st.ActiveSourceType == ActiveSourceTemporary &&
		(playbackState == StatePlaying || playbackState == StatePaused) &&
		currentItemID != "" && currentItemID != item.ID {
		var targetSortOrder int64
		originalOrder, targetSortOrder, reordered, err =
			m.store.moveTemporaryItemAfter(roomID, item.ID, currentItemID)
		if err != nil {
			return err
		}
		if reordered {
			// nextItem reads the in-memory row after the target finishes, so keep
			// it aligned with the order committed above.
			item.SortOrder = targetSortOrder
		}
	}

	playTarget := func() error {
		if activePlayer != nil {
			// Keep the existing LiveKit participant and published Opus track alive.
			// The player acknowledges only after the target is authoritative, so a
			// successful control response can never expose the previous track.
			return activePlayer.playNow(item)
		}
		return m.ensurePlayingItem(roomID, item)
	}
	if err := playTarget(); err != nil {
		if reordered {
			if restoreErr := m.store.restoreTemporaryQueueOrder(
				roomID,
				originalOrder,
			); restoreErr != nil {
				return fmt.Errorf(
					"%w (restore priority queue order: %v)",
					err,
					restoreErr,
				)
			}
		}
		return err
	}
	return nil
}

func (m *Manager) play(roomID string, resumeOnly bool) error {
	if pl := m.getPlayer(roomID); pl != nil {
		return pl.resume()
	}
	if resumeOnly {
		// Nothing playing and caller only asked to resume: start fresh.
	}
	return m.ensurePlaying(roomID)
}

// ensurePlaying starts a player for the room if one isn't running and there's
// at least one ready track. Safe to call repeatedly.
func (m *Manager) ensurePlaying(roomID string) error {
	return m.ensurePlayingItem(roomID, nil)
}

// ensurePlayingItem starts the player from preferred when supplied. Selecting
// the initial item before the goroutine starts makes a successful play_now
// response immediately consistent with State() and SSE snapshots.
func (m *Manager) ensurePlayingItem(roomID string, preferred *QueueItem) error {
	m.mu.Lock()
	if pl, ok := m.players[roomID]; ok {
		m.mu.Unlock()
		if preferred != nil {
			return pl.playNow(preferred)
		}
		// Already running. If it's idling on an empty queue, nudge it so it
		// re-checks and picks up the newly-ready track instead of waiting out
		// its idle timeout.
		pl.wake()
		return nil
	}
	// Reserve the slot under lock to avoid two concurrent starts.
	st, _ := m.store.getState(roomID)
	scope, snapshotID := playbackScope(st)
	first := preferred
	var err error
	if first == nil {
		first, err = m.store.firstPlayableInScope(roomID, -1, scope, snapshotID)
	}
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
		func(prev *QueueItem, transition playbackTransition, positionMS int64) *QueueItem {
			return m.nextItem(roomID, prev, transition, positionMS)
		},
		func() { m.persistAndNotify(roomID) },
		func() { m.persistAndNotifyForced(roomID) },
	)
	m.players[roomID] = pl
	m.mu.Unlock()

	if err := pl.connect(); err != nil {
		m.mu.Lock()
		delete(m.players, roomID)
		m.mu.Unlock()
		return err
	}
	// Publish an explicitly selected item only after LiveKit connected
	// successfully. Ordinary playback keeps the selector-driven startup path,
	// including resuming the persisted current item after a process restart.
	if preferred != nil {
		pl.setCurrent(preferred)
	}
	go func() {
		if preferred != nil {
			pl.runFrom(preferred)
		} else {
			pl.run()
		}
		m.mu.Lock()
		if m.players[roomID] == pl {
			delete(m.players, roomID)
		}
		m.mu.Unlock()
		m.persistAndNotify(roomID)
	}()
	return nil
}

// nextItem retains rows after playback, allowing previous/repeat modes and a
// restart-safe queue. The audio file remains a rebuildable cache entry.
func (m *Manager) nextItem(
	roomID string,
	prev *QueueItem,
	transition playbackTransition,
	positionMS int64,
) *QueueItem {
	st, _ := m.store.getState(roomID)
	mode := ModeSequential
	if st != nil {
		mode = NormalizePlaybackMode(string(st.PlaybackMode))
	}
	scope, snapshotID := playbackScope(st)
	if prev == nil {
		if st != nil && st.CurrentItemID != "" {
			if current, _ := m.store.getRoomItem(roomID, st.CurrentItemID); current != nil && current.Status == StatusReady {
				return current
			}
		}
		item, _ := m.store.firstPlayableInScope(
			roomID,
			m.cursorAfter(roomID, scope, snapshotID),
			scope,
			snapshotID,
		)
		return item
	}
	if transition == transitionPrevious && positionMS > 3000 {
		return prev
	}
	if mode == ModeShuffle {
		items, err := m.store.listScopedQueue(roomID, scope, snapshotID)
		if err != nil {
			return nil
		}
		ready := make([]*QueueItem, 0, len(items))
		for _, item := range items {
			if item.Status == StatusReady {
				ready = append(ready, item)
			}
		}
		if len(ready) == 0 {
			return nil
		}
		sort.SliceStable(ready, func(i, j int) bool {
			return musicBoxShuffleRank(roomID, ready[i].ID) <
				musicBoxShuffleRank(roomID, ready[j].ID)
		})
		currentIndex := 0
		for index, item := range ready {
			if item.ID == prev.ID {
				currentIndex = index
				break
			}
		}
		if transition == transitionPrevious {
			return ready[(currentIndex-1+len(ready))%len(ready)]
		}
		return ready[(currentIndex+1)%len(ready)]
	}
	if transition == transitionPrevious {
		if item, _ := m.store.lastPlayableBefore(
			roomID,
			prev.SortOrder,
			scope,
			snapshotID,
		); item != nil {
			return item
		}
		return prev
	}
	if transition == transitionNatural && mode == ModeRepeatOne {
		return prev
	}
	item, err := m.store.firstPlayableInScope(
		roomID,
		prev.SortOrder,
		scope,
		snapshotID,
	)
	if err != nil {
		return nil
	}
	if item == nil && mode == ModeRepeatAll {
		m.setCursor(roomID, scope, snapshotID, -1)
		item, _ = m.store.firstPlayableInScope(roomID, -1, scope, snapshotID)
	} else if mode != ModeShuffle {
		m.setCursor(roomID, scope, snapshotID, prev.SortOrder)
	}
	return item
}

func musicBoxShuffleRank(roomID, itemID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(roomID + "\x00" + itemID))
	return h.Sum64()
}

func (m *Manager) setPlaybackMode(roomID, value string) error {
	mode := NormalizePlaybackMode(value)
	if value != string(mode) {
		return fmt.Errorf("unknown playback mode %q", value)
	}
	m.persistMu.Lock()
	st, err := m.store.ensureState(roomID)
	if err == nil && st.ActiveSourceType == ActiveSourceTemporary &&
		(mode == ModeRepeatAll || mode == ModeShuffle) {
		err = fmt.Errorf("playback mode %q is unavailable for the temporary playlist", mode)
	}
	if err == nil && st.PlaybackMode != mode {
		st.PlaybackMode = mode
		st.Revision++
		err = m.store.saveState(*st)
	}
	m.persistMu.Unlock()
	if err == nil {
		m.notify(roomID)
	}
	return err
}

func (m *Manager) stopRoom(roomID string) error {
	pl := m.getPlayer(roomID)
	currentID := ""
	if pl != nil {
		_, currentID, _ = pl.snapshot()
		if err := pl.stop(); err != nil {
			return err
		}
	}
	m.persistMu.Lock()
	st, _ := m.store.getState(roomID)
	if st != nil {
		st.State = StateStopped
		if currentID != "" {
			st.CurrentItemID = currentID
		}
		st.PositionMS = 0
		st.Revision++
		_ = m.store.saveState(*st)
	}
	m.persistMu.Unlock()
	m.notify(roomID)
	return nil
}

func (m *Manager) getPlayer(roomID string) *player {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.players[roomID]
}

// persistAndNotify writes the live player snapshot to the DB and fans out.
func (m *Manager) persistAndNotify(roomID string) {
	m.persistPlayerStateAndNotify(roomID, false)
}

func (m *Manager) persistAndNotifyForced(roomID string) {
	m.persistPlayerStateAndNotify(roomID, true)
}

func (m *Manager) persistPlayerStateAndNotify(roomID string, forceRevision bool) {
	m.persistMu.Lock()
	pl := m.getPlayer(roomID)
	st, _ := m.store.ensureState(roomID)
	if st == nil {
		st = &RoomState{RoomID: roomID, Volume: 100, PlaybackMode: ModeSequential}
	}
	if pl != nil {
		state, currentID, pos := pl.snapshot()
		structuralChange := st.State != state || st.CurrentItemID != currentID
		st.State = state
		st.CurrentItemID = currentID
		st.PositionMS = pos
		if structuralChange || forceRevision {
			st.Revision++
		}
	} else {
		structuralChange := st.State != StateStopped
		st.State = StateStopped
		st.PositionMS = 0
		if structuralChange {
			st.Revision++
		}
	}
	_ = m.store.saveState(*st)
	m.persistMu.Unlock()
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
	item, err := m.store.deleteRoomItem(roomID, itemID)
	if err != nil {
		return err
	}
	if item != nil && item.FilePath != "" {
		_ = os.Remove(item.FilePath)
	}
	if playingCurrent && pl != nil {
		if err := pl.skip(); err != nil {
			return err
		}
	}
	m.bumpRevision(roomID)
	// Removing a ready track frees disk space; let a pending track download.
	go m.pumpRoom(roomID)
	return nil
}

// ActivatePlaylist snapshots a saved playlist into an independent active
// queue.  The room's temporary requests remain untouched and are restored by
// switching back to ActiveSourceTemporary.
func (m *Manager) ActivatePlaylist(
	roomID string,
	sourceType ActiveSourceType,
	playlistID, playlistName, ownerUserID string,
	playlistCreatedAt int64,
	actorUserID string,
	tracks []SnapshotTrack,
	startPlaying bool,
) error {
	if !m.Enabled() {
		return ErrUnavailable
	}
	if sourceType != ActiveSourceTemporary &&
		sourceType != ActiveSourceRoomPlaylist &&
		sourceType != ActiveSourceUserPlaylist {
		return fmt.Errorf("unknown active source %q", sourceType)
	}
	lock := m.controlLock(roomID)
	lock.Lock()
	defer lock.Unlock()

	if player := m.getPlayer(roomID); player != nil {
		if err := player.stop(); err != nil {
			return err
		}
	}

	snapshotID := ""
	if sourceType != ActiveSourceTemporary {
		snapshotID = "mbs_" + randomID()
		for index, track := range tracks {
			if _, err := m.store.insertItem(QueueItem{
				ID:            "mbx_" + randomID(),
				RoomID:        roomID,
				Source:        track.Source,
				TrackID:       track.TrackID,
				Title:         track.Title,
				Artist:        track.Artist,
				DurationMS:    track.DurationMS,
				Status:        StatusPending,
				AddedByUserID: actorUserID,
				QueueScope:    QueueScopeSavedPlaylistSnapshot,
				SnapshotID:    snapshotID,
				SortOrder:     int64(index+1) * 10,
			}); err != nil {
				_, _ = m.store.deleteSavedSnapshot(roomID, snapshotID)
				return err
			}
		}
	}

	m.persistMu.Lock()
	st, err := m.store.ensureState(roomID)
	if err == nil {
		st.State = StateStopped
		st.CurrentItemID = ""
		st.PositionMS = 0
		st.ActiveSourceType = sourceType
		st.ActivePlaylistID = playlistID
		st.ActivePlaylistName = playlistName
		st.ActivePlaylistOwnerID = ownerUserID
		st.ActivePlaylistCreatedAt = playlistCreatedAt
		st.ActiveSnapshotID = snapshotID
		if sourceType == ActiveSourceTemporary &&
			(st.PlaybackMode == ModeRepeatAll || st.PlaybackMode == ModeShuffle) {
			st.PlaybackMode = ModeSequential
		}
		st.Revision++
		err = m.store.saveState(*st)
	}
	m.persistMu.Unlock()
	if err != nil {
		if snapshotID != "" {
			_, _ = m.store.deleteSavedSnapshot(roomID, snapshotID)
		}
		return err
	}
	activeScope := QueueScopeTemporary
	if sourceType != ActiveSourceTemporary {
		activeScope = QueueScopeSavedPlaylistSnapshot
	}
	m.setCursor(roomID, activeScope, snapshotID, -1)
	removed, cleanupErr := m.store.deleteSavedSnapshotsExcept(roomID, snapshotID)
	if cleanupErr != nil {
		log.Printf("musicbox: room %s failed to clean obsolete playlist snapshots: %v", roomID, cleanupErr)
	} else {
		for _, item := range removed {
			if item.FilePath != "" {
				_ = os.Remove(item.FilePath)
			}
		}
	}
	m.notify(roomID)
	go m.pumpRoom(roomID)
	if startPlaying {
		go m.ensurePlayingAfterStop(roomID)
	}
	return nil
}

func (m *Manager) ensurePlayingAfterStop(roomID string) {
	if err := m.waitForPlayerStop(roomID); err != nil {
		log.Printf("musicbox: room %s player cleanup failed: %v", roomID, err)
		return
	}
	if err := m.ensurePlaying(roomID); err != nil {
		log.Printf("musicbox: room %s restart after stop failed: %v", roomID, err)
	}
}

func (m *Manager) waitForPlayerStop(roomID string) error {
	for attempt := 0; attempt < 40; attempt++ {
		if m.getPlayer(roomID) == nil {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("music box player cleanup timed out")
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
	scope, snapshotID := playbackScope(st)
	items, err := m.store.listScopedQueue(roomID, scope, snapshotID)
	if err != nil {
		return nil, nil, err
	}
	return st, items, nil
}

func (m *Manager) TemporaryQueue(roomID string) ([]*QueueItem, error) {
	return m.store.listScopedQueue(roomID, QueueScopeTemporary, "")
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
