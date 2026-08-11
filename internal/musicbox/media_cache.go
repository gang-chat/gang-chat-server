package musicbox

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultBroadcastCacheMaxBytes int64 = 2 << 30
	broadcastPrefetchWindow             = 3
)

// broadcastCacheKey is stable across rooms and queue-row lifetimes. Encoding
// settings are part of the key so changing the server's Opus bitrate never
// serves an old incompatible artifact as a cache hit.
func broadcastCacheKey(source, trackID, opusBitrate string) string {
	sum := sha256.Sum256([]byte(
		strings.TrimSpace(source) + "\x00" +
			strings.TrimSpace(trackID) + "\x00ogg-opus\x00" +
			strings.TrimSpace(opusBitrate),
	))
	return fmt.Sprintf("%x", sum[:16])
}

func (m *Manager) broadcastCacheDir() string {
	return filepath.Join(m.cfg.Dir, "broadcast-cache")
}

func (m *Manager) broadcastCachePath(item *QueueItem) string {
	return filepath.Join(
		m.broadcastCacheDir(),
		broadcastCacheKey(item.Source, item.TrackID, m.cfg.OpusBitrate)+".ogg",
	)
}

// prepareBroadcastMedia serializes preparation by cache key across rooms,
// publishes through an atomic rename, and attaches the shared artifact to the
// queue row. A cache hit does not resolve a fresh upstream URL.
func (m *Manager) prepareBroadcastMedia(
	ctx context.Context,
	item *QueueItem,
) (*transcodeResult, string, error) {
	dst := m.broadcastCachePath(item)
	lock := m.controlLock("broadcast-cache:" + filepath.Base(dst))
	lock.Lock()
	defer lock.Unlock()

	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		m.obs.cacheHits.Add(1)
		now := time.Now()
		_ = os.Chtimes(dst, now, now)
		duration := item.DurationMS
		if cachedDuration, lookupErr := m.store.cachedMediaDuration(dst); lookupErr == nil && cachedDuration > 0 {
			duration = cachedDuration
		}
		return &transcodeResult{SizeBytes: info.Size(), DurationMS: duration}, dst, nil
	}
	m.obs.cacheMisses.Add(1)

	if err := os.MkdirAll(m.broadcastCacheDir(), 0o755); err != nil {
		return nil, "", fmt.Errorf("prepare broadcast cache dir: %w", err)
	}
	resolvedURL, err := m.resolveURL(ctx, item)
	if err != nil {
		return nil, "", fmt.Errorf("resolve url: %w", err)
	}
	tmp := dst + ".tmp-" + randomID()
	result, err := m.tc.transcode(ctx, item.Source, resolvedURL, tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return nil, "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		// Another process may have won the deterministic publish race. Reuse its
		// complete artifact instead of replacing it or failing this queue item.
		if info, statErr := os.Stat(dst); statErr == nil && info.Size() > 0 {
			_ = os.Remove(tmp)
			result.SizeBytes = info.Size()
			return result, dst, nil
		}
		_ = os.Remove(tmp)
		return nil, "", fmt.Errorf("publish broadcast cache: %w", err)
	}
	return result, dst, nil
}

// acquireMediaLease protects a file from LRU cleanup for the complete period
// in which a player may read it. The returned release function is idempotent.
func (m *Manager) acquireMediaLease(item *QueueItem) func() {
	if item == nil || strings.TrimSpace(item.FilePath) == "" {
		return func() {}
	}
	path := filepath.Clean(item.FilePath)
	m.cacheMu.Lock()
	if m.cacheLeases == nil {
		m.cacheLeases = map[string]int{}
	}
	m.cacheLeases[path]++
	m.cacheMu.Unlock()
	now := time.Now()
	_ = os.Chtimes(path, now, now)

	var once sync.Once
	return func() {
		once.Do(func() {
			becameUnleased := false
			m.cacheMu.Lock()
			if m.cacheLeases[path] <= 1 {
				delete(m.cacheLeases, path)
				becameUnleased = true
			} else {
				m.cacheLeases[path]--
			}
			m.cacheMu.Unlock()
			if becameUnleased {
				go m.cleanupBroadcastCache("")
			}
		})
	}
}

func (m *Manager) mediaPathLeased(path string) bool {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	return m.cacheLeases[filepath.Clean(path)] > 0
}

// cleanupBroadcastCache applies filesystem-mtime LRU to the shared hot cache.
// Queue metadata is repaired before a removed artifact can be selected again.
func (m *Manager) cleanupBroadcastCache(keepPath string) {
	if m == nil || m.cfg.CacheMaxBytes <= 0 {
		return
	}
	m.cacheCleanupMu.Lock()
	defer m.cacheCleanupMu.Unlock()

	entries, err := os.ReadDir(m.broadcastCacheDir())
	if err != nil {
		return
	}
	type candidate struct {
		path    string
		size    int64
		modTime time.Time
	}
	files := make([]candidate, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ogg") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || info.Size() <= 0 {
			continue
		}
		path := filepath.Join(m.broadcastCacheDir(), entry.Name())
		files = append(files, candidate{path: path, size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
	}
	if total <= m.cfg.CacheMaxBytes {
		return
	}
	target := m.cfg.CacheMaxBytes * 9 / 10
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for _, file := range files {
		if total <= target {
			break
		}
		if filepath.Clean(file.path) == filepath.Clean(keepPath) || m.mediaPathLeased(file.path) {
			continue
		}
		current, currentErr := m.store.mediaPathIsCurrent(file.path)
		if currentErr != nil || current {
			continue
		}
		roomIDs, repairErr := m.store.markMediaPathMissing(file.path)
		if repairErr != nil {
			continue
		}
		if err := os.Remove(file.path); err != nil {
			// The rows are intentionally pending now. Their next preparation pass
			// will see the still-complete file and attach it without downloading.
			for _, roomID := range roomIDs {
				go m.pumpRoom(roomID)
			}
			continue
		}
		total -= file.size
		m.obs.cacheEvictions.Add(1)
		m.obs.cacheEvictedBytes.Add(file.size)
		log.Printf(
			"musicbox cache evicted cache_key=%q bytes=%d repaired_rooms=%d remaining_bytes=%d",
			strings.TrimSuffix(filepath.Base(file.path), filepath.Ext(file.path)),
			file.size, len(roomIDs), total,
		)
		for _, roomID := range roomIDs {
			m.bumpRevision(roomID)
			go m.pumpRoom(roomID)
		}
	}
}

func (m *Manager) isSharedBroadcastPath(path string) bool {
	rel, err := filepath.Rel(m.broadcastCacheDir(), filepath.Clean(path))
	return err == nil && rel != "." && rel != "" && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// releaseQueueMedia detaches queue-row ownership from media. Shared artifacts
// remain available for other rooms and are reclaimed only by LRU; legacy
// per-room files are removed once no row and no player references them.
func (m *Manager) releaseQueueMedia(items []*QueueItem) {
	seen := map[string]struct{}{}
	for _, item := range items {
		if item == nil || strings.TrimSpace(item.FilePath) == "" {
			continue
		}
		path := filepath.Clean(item.FilePath)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		if m.isSharedBroadcastPath(path) || m.mediaPathLeased(path) {
			continue
		}
		refs, err := m.store.countMediaPathReferences(path)
		if err == nil && refs == 0 {
			_ = os.Remove(path)
		}
	}
	go m.cleanupBroadcastCache("")
}

// prefetchWindow returns at most current+next-two rows in actual playback
// order. It is pure so queue edits, wrap and shuffle boundaries can be tested
// without starting download goroutines.
func prefetchWindow(
	items []*QueueItem,
	currentItemID string,
	afterSort int64,
	mode PlaybackMode,
	roomID string,
	limit int,
) []*QueueItem {
	if limit <= 0 || len(items) == 0 {
		return nil
	}
	ordered := append([]*QueueItem(nil), items...)
	if mode == ModeShuffle {
		sort.SliceStable(ordered, func(i, j int) bool {
			return musicBoxShuffleRank(roomID, ordered[i].ID) < musicBoxShuffleRank(roomID, ordered[j].ID)
		})
	}
	start := -1
	if currentItemID != "" {
		for index, item := range ordered {
			if item.ID == currentItemID {
				start = index
				break
			}
		}
	}
	if start < 0 {
		for index, item := range ordered {
			if item.SortOrder > afterSort {
				start = index
				break
			}
		}
	}
	if start < 0 {
		if mode != ModeRepeatAll && mode != ModeShuffle {
			return nil
		}
		start = 0
	}
	window := make([]*QueueItem, 0, limit)
	for offset := 0; offset < len(ordered) && len(window) < limit; offset++ {
		index := start + offset
		if index >= len(ordered) {
			if mode != ModeRepeatAll && mode != ModeShuffle {
				break
			}
			index %= len(ordered)
		}
		window = append(window, ordered[index])
	}
	return window
}

func (m *Manager) nextPendingPrefetch(roomID string, st *RoomState) (*QueueItem, error) {
	scope, snapshotID := playbackScope(st)
	items, err := m.store.listScopedQueue(roomID, scope, snapshotID)
	if err != nil {
		return nil, err
	}
	mode := ModeSequential
	currentItemID := ""
	if st != nil {
		mode = NormalizePlaybackMode(string(st.PlaybackMode))
		currentItemID = st.CurrentItemID
	}
	for _, item := range prefetchWindow(
		items,
		currentItemID,
		m.cursorAfter(roomID, scope, snapshotID),
		mode,
		roomID,
		broadcastPrefetchWindow,
	) {
		if item.Status == StatusPending {
			return item, nil
		}
	}
	return nil, nil
}
