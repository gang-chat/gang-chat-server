package musicbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMusicBoxPrepareErrorKindDoesNotExposeErrorDetails(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{err: context.DeadlineExceeded, want: "timeout"},
		{err: context.Canceled, want: "cancelled"},
		{err: errors.New("resolve url: private upstream detail"), want: "resolve"},
		{err: errors.New("ffmpeg: https://signed.example/token"), want: "transcode"},
		{err: errors.New("publish broadcast cache: access denied"), want: "cache_publish"},
	}
	for _, test := range cases {
		if got := musicBoxPrepareErrorKind(test.err); got != test.want {
			t.Fatalf("error kind for %q = %q, want %q", test.err, got, test.want)
		}
	}
}

func TestObservabilitySnapshotIncludesCountersPlayersAndCacheUsage(t *testing.T) {
	dir := t.TempDir()
	manager := &Manager{
		cfg:     Config{Dir: dir},
		players: map[string]*player{"room": {}},
	}
	if err := os.MkdirAll(manager.broadcastCacheDir(), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(manager.broadcastCacheDir(), "track.ogg"),
		[]byte("opus"),
		0o600,
	); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	manager.obs.cacheHits.Add(2)
	manager.obs.controlAttempts.Add(3)
	manager.RecordFullSnapshotEvent()
	manager.RecordCompactProgressEvent()

	snapshot := manager.ObservabilitySnapshot()
	if snapshot.ActivePlayers != 1 ||
		snapshot.BroadcastCacheFiles != 1 ||
		snapshot.BroadcastCacheBytes != 4 ||
		snapshot.CacheHits != 2 ||
		snapshot.ControlAttempts != 3 ||
		snapshot.FullSnapshotEvents != 1 ||
		snapshot.CompactProgressEvents != 1 {
		t.Fatalf("unexpected observability snapshot: %+v", snapshot)
	}
}
