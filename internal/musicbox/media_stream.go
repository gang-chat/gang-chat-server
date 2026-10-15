package musicbox

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// streamPollInterval is how often a reader waiting on a growing transcode
	// checks for new bytes. ffmpeg runs far ahead of real time and the player
	// reads ahead of its pacing, so this only shows up in startup latency.
	streamPollInterval = 20 * time.Millisecond
	// streamPlayablePages is the number of complete Ogg pages that make an
	// in-progress transcode playable: the OpusHead and OpusTags headers that
	// oggreader.NewOggReader consumes, plus the first page of audio. Waiting
	// for audio means a failed resolve or upstream connection never makes a
	// row look playable.
	streamPlayablePages = 3
)

var errStreamUnavailable = errors.New("music box stream is no longer available")

// mediaStream is a broadcast artifact that ffmpeg is still writing. Players
// read the temporary file through a followReader, so a cold track starts as
// soon as its first Ogg pages exist instead of after the whole download.
//
// Streams are keyed by their final cache path, which is shared across rooms:
// a row in another room that is waiting on the same track's cache lock can
// play the same in-progress file.
type mediaStream struct {
	tmpPath   string
	finalPath string
	ownerID   string // queue item whose process runs the transcode
	startedAt time.Time

	playable     chan struct{} // closed once streamPlayablePages pages are written
	playableOnce sync.Once
	done         chan struct{} // closed once the transcode succeeded or failed

	mu        sync.Mutex
	published bool // tmpPath was renamed to finalPath
	err       error
}

func newMediaStream(tmpPath, finalPath, ownerID string) *mediaStream {
	return &mediaStream{
		tmpPath:   tmpPath,
		finalPath: finalPath,
		ownerID:   ownerID,
		startedAt: time.Now(),
		playable:  make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// markPlayable records that the headers and first audio page are on disk.
// It reports whether this call made the stream playable.
func (s *mediaStream) markPlayable() bool {
	marked := false
	s.playableOnce.Do(func() {
		close(s.playable)
		marked = true
	})
	return marked
}

// isPlayable reports whether a player may start reading the stream now.
func (s *mediaStream) isPlayable() bool {
	select {
	case <-s.playable:
	default:
		return false
	}
	return s.failure() == nil
}

func (s *mediaStream) isFinished() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *mediaStream) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// fail ends the stream with err. Readers drain what was written, then
// surface err instead of io.EOF.
func (s *mediaStream) fail(err error) {
	if err == nil {
		err = errStreamUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isFinished() {
		return
	}
	s.err = err
	close(s.done)
}

// publish runs the atomic rename into the cache and ends the stream with its
// result. Holding mu across the rename keeps open() from choosing a path that
// is about to disappear.
func (s *mediaStream) publish(rename func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isFinished() {
		if s.err != nil {
			return s.err
		}
		return nil
	}
	if err := rename(); err != nil {
		s.err = err
		close(s.done)
		return err
	}
	s.published = true
	close(s.done)
	return nil
}

// open returns a descriptor for the stream's bytes. Before publication that
// is the temporary file; the descriptor keeps reading the same inode after
// the rename (POSIX), so a reader never has to switch files mid-track.
func (s *mediaStream) open() (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	if s.published {
		return os.Open(s.finalPath)
	}
	return os.Open(s.tmpPath)
}

// followReader reads a file that another process is still appending to. At
// the current end of the file it waits for more bytes instead of returning
// io.EOF, until the stream finishes: then it reports io.EOF after a success
// or the stream's error after a failure. Cancelling ctx unblocks a waiting
// Read with ctx.Err().
type followReader struct {
	ctx    context.Context
	file   io.Reader
	stream *mediaStream
	poll   time.Duration
}

func newFollowReader(ctx context.Context, file io.Reader, stream *mediaStream) *followReader {
	return &followReader{ctx: ctx, file: file, stream: stream, poll: streamPollInterval}
}

func (r *followReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		// Observe completion before reading: if the stream had already finished,
		// every byte was written before this read, so an EOF here is final.
		finished := r.stream.isFinished()
		n, err := r.file.Read(p)
		if n > 0 {
			return n, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if finished {
			if streamErr := r.stream.failure(); streamErr != nil {
				return 0, streamErr
			}
			return 0, io.EOF
		}
		timer := time.NewTimer(r.poll)
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return 0, r.ctx.Err()
		case <-r.stream.done:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// completeOggPages counts the whole Ogg pages at the start of r, up to limit.
// It stops at the first page that has not been completely written yet.
func completeOggPages(r io.Reader, limit int) int {
	reader := bufio.NewReader(r)
	header := make([]byte, 27)
	for pages := 0; pages < limit; pages++ {
		if _, err := io.ReadFull(reader, header); err != nil {
			return pages
		}
		if string(header[:4]) != "OggS" {
			return pages
		}
		segments := make([]byte, header[26])
		if _, err := io.ReadFull(reader, segments); err != nil {
			return pages
		}
		payload := 0
		for _, size := range segments {
			payload += int(size)
		}
		if _, err := reader.Discard(payload); err != nil {
			return pages
		}
	}
	return limit
}

func oggFilePlayable(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	return completeOggPages(file, streamPlayablePages) >= streamPlayablePages
}

// beginStream registers an in-progress transcode for its final cache path,
// replacing a finished stream left behind for the same path. ownerID is the
// queue item whose process runs the transcode and retires the stream.
func (m *Manager) beginStream(tmpPath, finalPath, ownerID string) *mediaStream {
	stream := newMediaStream(tmpPath, filepath.Clean(finalPath), ownerID)
	m.streamsMu.Lock()
	if m.streams == nil {
		m.streams = map[string]*mediaStream{}
	}
	m.streams[stream.finalPath] = stream
	m.streamsMu.Unlock()
	return stream
}

// retireStream forgets ownerID's finished stream once the owner's queue row
// records the outcome, so the row never looks unplayable between the two.
// Another row's stream for the same path (a newer transcode, or one this row
// only waited on) is left to its own owner.
func (m *Manager) retireStream(finalPath, ownerID string) {
	finalPath = filepath.Clean(finalPath)
	m.streamsMu.Lock()
	defer m.streamsMu.Unlock()
	if stream := m.streams[finalPath]; stream != nil &&
		stream.ownerID == ownerID && stream.isFinished() {
		delete(m.streams, finalPath)
	}
}

func (m *Manager) streamFor(finalPath string) *mediaStream {
	m.streamsMu.Lock()
	defer m.streamsMu.Unlock()
	return m.streams[filepath.Clean(finalPath)]
}

// playableStream returns the in-progress transcode a downloading row can be
// played from, or nil.
func (m *Manager) playableStream(item *QueueItem) *mediaStream {
	if item == nil || item.Status != StatusDownloading {
		return nil
	}
	stream := m.streamFor(m.broadcastCachePath(item))
	if stream == nil || !stream.isPlayable() {
		return nil
	}
	return stream
}

// streamable is the store's streamingFilter: a downloading row is playable
// once its transcode has written its first audio page.
func (m *Manager) streamable(item *QueueItem) bool {
	return m.playableStream(item) != nil
}

// ItemPlayable reports whether the room player can start item right now:
// its audio is fully prepared, or it is still downloading but the transcode
// has already written enough to stream from. Pending and failed rows are
// never playable.
func (m *Manager) ItemPlayable(item *QueueItem) bool {
	if m == nil || item == nil {
		return false
	}
	switch item.Status {
	case StatusReady:
		return true
	case StatusDownloading:
		return m.playableStream(item) != nil
	default:
		return false
	}
}

// watchStream marks a stream playable as soon as its first audio page is on
// disk, then starts the room playing it if nothing else is. A stream that
// finishes first is left to the ordinary completion path.
func (m *Manager) watchStream(ctx context.Context, stream *mediaStream, roomID string) {
	ticker := time.NewTicker(streamPollInterval)
	defer ticker.Stop()
	for {
		if oggFilePlayable(stream.tmpPath) {
			if stream.markPlayable() && stream.isPlayable() {
				m.obs.streamStarts.Add(1)
				m.obs.streamStartDurationMS.Add(time.Since(stream.startedAt).Milliseconds())
				m.bumpRevision(roomID)
				if err := m.ensurePlaying(roomID); err != nil {
					log.Printf("musicbox: room %s start streaming playback failed: %v", roomID, err)
				}
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-stream.done:
			return
		case <-ticker.C:
		}
	}
}

// leasedMedia is the reader a player gets for one track. Closing it closes
// the file and releases the cache lease on the artifact's final path.
type leasedMedia struct {
	io.Reader
	file    *os.File
	release func()
}

func (l *leasedMedia) Close() error {
	err := l.file.Close()
	l.release()
	return err
}

// openBroadcastMedia opens the audio a player should read for item: the
// complete cache file once the row is ready, or the growing temporary file of
// an in-flight transcode. Either way the reader holds a cache lease on the
// final path until it is closed, so LRU cleanup can't evict the artifact
// mid-track, including right after a stream is renamed into the cache.
func (m *Manager) openBroadcastMedia(ctx context.Context, item *QueueItem) (io.ReadCloser, error) {
	if item.Status != StatusReady || item.FilePath == "" {
		if stream := m.playableStream(item); stream != nil {
			return m.openStream(ctx, stream)
		}
		// The player's copy of the row can be stale: a stream it was selected
		// for may have finished, or repeat/previous may replay an item that was
		// streaming the first time round.
		fresh, err := m.store.getRoomItem(item.RoomID, item.ID)
		if err != nil {
			return nil, err
		}
		if fresh == nil {
			return nil, ErrQueueItemNotFound
		}
		if stream := m.playableStream(fresh); stream != nil {
			return m.openStream(ctx, stream)
		}
		if fresh.Status != StatusReady || fresh.FilePath == "" {
			return nil, ErrQueueItemNotReady
		}
		item = fresh
	}
	release := m.acquireMediaLease(item)
	file, err := os.Open(item.FilePath)
	if err != nil {
		release()
		return nil, err
	}
	return &leasedMedia{Reader: file, file: file, release: release}, nil
}

func (m *Manager) openStream(ctx context.Context, stream *mediaStream) (io.ReadCloser, error) {
	release := m.acquireMediaLease(&QueueItem{FilePath: stream.finalPath})
	file, err := stream.open()
	if err != nil {
		release()
		return nil, err
	}
	return &leasedMedia{
		Reader:  newFollowReader(ctx, file, stream),
		file:    file,
		release: release,
	}, nil
}
