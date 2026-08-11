package musicbox

import (
	"context"
	"log"
	"os"
	"sync/atomic"
	"time"
)

// ObservabilitySnapshot is intentionally dependency-free so operators get
// useful music-box telemetry even when the server has no Prometheus exporter.
// It is emitted as structured key/value logs and is also available to tests or
// a future authenticated diagnostics endpoint.
type ObservabilitySnapshot struct {
	ActivePlayers         int   `json:"active_players"`
	BroadcastCacheFiles   int   `json:"broadcast_cache_files"`
	BroadcastCacheBytes   int64 `json:"broadcast_cache_bytes"`
	CacheHits             int64 `json:"cache_hits"`
	CacheMisses           int64 `json:"cache_misses"`
	CacheEvictions        int64 `json:"cache_evictions"`
	CacheEvictedBytes     int64 `json:"cache_evicted_bytes"`
	PrepareSuccesses      int64 `json:"prepare_successes"`
	PrepareFailures       int64 `json:"prepare_failures"`
	PrepareDurationMS     int64 `json:"prepare_duration_ms"`
	ControlAttempts       int64 `json:"control_attempts"`
	ControlSuccesses      int64 `json:"control_successes"`
	ControlFailures       int64 `json:"control_failures"`
	RevisionConflicts     int64 `json:"revision_conflicts"`
	ControlDurationMS     int64 `json:"control_duration_ms"`
	EmptyTimersStarted    int64 `json:"empty_timers_started"`
	EmptyTimersCancelled  int64 `json:"empty_timers_cancelled"`
	EmptyRoomsExpired     int64 `json:"empty_rooms_expired"`
	EmptyCleanupRetries   int64 `json:"empty_cleanup_retries"`
	FullSnapshotEvents    int64 `json:"full_snapshot_events"`
	CompactProgressEvents int64 `json:"compact_progress_events"`
}

type observabilityCounters struct {
	cacheHits             atomic.Int64
	cacheMisses           atomic.Int64
	cacheEvictions        atomic.Int64
	cacheEvictedBytes     atomic.Int64
	prepareSuccesses      atomic.Int64
	prepareFailures       atomic.Int64
	prepareDurationMS     atomic.Int64
	controlAttempts       atomic.Int64
	controlSuccesses      atomic.Int64
	controlFailures       atomic.Int64
	revisionConflicts     atomic.Int64
	controlDurationMS     atomic.Int64
	emptyTimersStarted    atomic.Int64
	emptyTimersCancelled  atomic.Int64
	emptyRoomsExpired     atomic.Int64
	emptyCleanupRetries   atomic.Int64
	fullSnapshotEvents    atomic.Int64
	compactProgressEvents atomic.Int64
}

func (m *Manager) ObservabilitySnapshot() ObservabilitySnapshot {
	if m == nil {
		return ObservabilitySnapshot{}
	}
	m.mu.Lock()
	activePlayers := len(m.players)
	m.mu.Unlock()
	files, bytes := m.broadcastCacheUsage()
	return ObservabilitySnapshot{
		ActivePlayers:         activePlayers,
		BroadcastCacheFiles:   files,
		BroadcastCacheBytes:   bytes,
		CacheHits:             m.obs.cacheHits.Load(),
		CacheMisses:           m.obs.cacheMisses.Load(),
		CacheEvictions:        m.obs.cacheEvictions.Load(),
		CacheEvictedBytes:     m.obs.cacheEvictedBytes.Load(),
		PrepareSuccesses:      m.obs.prepareSuccesses.Load(),
		PrepareFailures:       m.obs.prepareFailures.Load(),
		PrepareDurationMS:     m.obs.prepareDurationMS.Load(),
		ControlAttempts:       m.obs.controlAttempts.Load(),
		ControlSuccesses:      m.obs.controlSuccesses.Load(),
		ControlFailures:       m.obs.controlFailures.Load(),
		RevisionConflicts:     m.obs.revisionConflicts.Load(),
		ControlDurationMS:     m.obs.controlDurationMS.Load(),
		EmptyTimersStarted:    m.obs.emptyTimersStarted.Load(),
		EmptyTimersCancelled:  m.obs.emptyTimersCancelled.Load(),
		EmptyRoomsExpired:     m.obs.emptyRoomsExpired.Load(),
		EmptyCleanupRetries:   m.obs.emptyCleanupRetries.Load(),
		FullSnapshotEvents:    m.obs.fullSnapshotEvents.Load(),
		CompactProgressEvents: m.obs.compactProgressEvents.Load(),
	}
}

func (m *Manager) broadcastCacheUsage() (files int, bytes int64) {
	entries, err := os.ReadDir(m.broadcastCacheDir())
	if err != nil {
		return 0, 0
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Size() <= 0 {
			continue
		}
		files++
		bytes += info.Size()
	}
	return files, bytes
}

// RunObservabilityLog emits a cumulative snapshot at a low frequency. The
// caller owns ctx so tests and future graceful shutdown can stop it cleanly.
func (m *Manager) RunObservabilityLog(ctx context.Context, interval time.Duration) {
	if m == nil || !m.Enabled() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s := m.ObservabilitySnapshot()
			log.Printf(
				"musicbox metrics active_players=%d cache_files=%d cache_bytes=%d cache_hits=%d cache_misses=%d cache_evictions=%d cache_evicted_bytes=%d prepare_successes=%d prepare_failures=%d prepare_duration_ms=%d control_attempts=%d control_successes=%d control_failures=%d revision_conflicts=%d control_duration_ms=%d empty_timers_started=%d empty_timers_cancelled=%d empty_rooms_expired=%d empty_cleanup_retries=%d full_snapshot_events=%d compact_progress_events=%d",
				s.ActivePlayers, s.BroadcastCacheFiles, s.BroadcastCacheBytes,
				s.CacheHits, s.CacheMisses, s.CacheEvictions, s.CacheEvictedBytes,
				s.PrepareSuccesses, s.PrepareFailures, s.PrepareDurationMS,
				s.ControlAttempts, s.ControlSuccesses, s.ControlFailures,
				s.RevisionConflicts, s.ControlDurationMS,
				s.EmptyTimersStarted, s.EmptyTimersCancelled,
				s.EmptyRoomsExpired, s.EmptyCleanupRetries,
				s.FullSnapshotEvents, s.CompactProgressEvents,
			)
		}
	}
}

func (m *Manager) RecordFullSnapshotEvent() {
	if m != nil {
		m.obs.fullSnapshotEvents.Add(1)
	}
}

func (m *Manager) RecordCompactProgressEvent() {
	if m != nil {
		m.obs.compactProgressEvents.Add(1)
	}
}
