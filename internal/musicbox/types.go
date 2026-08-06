// Package musicbox implements a room-level, server-side music player.
//
// Unlike the client-coordinated music_* feature (where each listener plays a
// track locally and syncs playback state), the music box does the playback on
// the server: it downloads each queued track, transcodes it to Opus, and
// broadcasts a single audio track into the room's LiveKit session through a
// dedicated bot participant. Listeners just hear it like any other speaker.
//
// Pipeline:
//
//	enqueue ──▶ transcode worker pool ──▶ .ogg on disk
//	                                          │
//	      player (one per active room) ◀──────┘
//	          ConnectToRoom + PublishTrack(Opus), reads pages from disk,
//	          pause/resume by gating the read loop.
//
// The queue is bounded by total transcoded bytes per room (config), not item
// count: adding a track that would exceed the cap is rejected.
package musicbox

// QueueStatus is the lifecycle of a queued track's audio file.
type QueueStatus string

const (
	StatusPending     QueueStatus = "pending"
	StatusDownloading QueueStatus = "downloading"
	StatusReady       QueueStatus = "ready"
	StatusFailed      QueueStatus = "failed"
)

type QueueScope string

const (
	QueueScopeTemporary             QueueScope = "temporary"
	QueueScopeSavedPlaylistSnapshot QueueScope = "saved_playlist_snapshot"
)

type ActiveSourceType string

const (
	ActiveSourceTemporary    ActiveSourceType = "temporary"
	ActiveSourceRoomPlaylist ActiveSourceType = "room_playlist"
	ActiveSourceUserPlaylist ActiveSourceType = "user_playlist"
)

// PlaybackState is the room player's state.
type PlaybackState string

const (
	StateStopped PlaybackState = "stopped"
	StatePlaying PlaybackState = "playing"
	StatePaused  PlaybackState = "paused"
)

// PlaybackMode is selected by the server and shared by every listener.  Keep
// the string values stable: older clients ignore the field while newer
// clients render the mode selector from it.
type PlaybackMode string

const (
	ModeSequential PlaybackMode = "sequential"
	ModeRepeatOne  PlaybackMode = "repeat_one"
	ModeRepeatAll  PlaybackMode = "repeat_all"
	ModeShuffle    PlaybackMode = "shuffle"
)

func NormalizePlaybackMode(value string) PlaybackMode {
	switch PlaybackMode(value) {
	case ModeRepeatOne, ModeRepeatAll, ModeShuffle:
		return PlaybackMode(value)
	default:
		return ModeSequential
	}
}

// QueueItem is one row of a room's music box queue.
type QueueItem struct {
	ID            string
	RoomID        string
	Source        string
	TrackID       string
	Title         string
	Artist        string
	DurationMS    int64
	Status        QueueStatus
	FilePath      string
	FileSizeBytes int64
	Error         string
	AddedByUserID string
	QueueScope    QueueScope
	SnapshotID    string
	SortOrder     int64
	CreatedAt     int64
	UpdatedAt     int64
}

// RoomState is the persisted playback state for a room.
type RoomState struct {
	RoomID                  string
	State                   PlaybackState
	CurrentItemID           string
	PositionMS              int64
	Volume                  int
	Revision                int64
	PlaybackMode            PlaybackMode
	ActiveSourceType        ActiveSourceType
	ActivePlaylistID        string
	ActivePlaylistName      string
	ActivePlaylistOwnerID   string
	ActivePlaylistCreatedAt int64
	ActiveSnapshotID        string
	UpdatedAt               int64
}

type SnapshotTrack struct {
	Source     string
	TrackID    string
	Title      string
	Artist     string
	DurationMS int64
}
