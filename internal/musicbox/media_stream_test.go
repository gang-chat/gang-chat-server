package musicbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Ogg/Opus fixtures. oggreader verifies page checksums, so the pages are real.

func testOggCRC(data []byte) uint32 {
	var table [256]uint32
	for i := range table {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = (r << 1) ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		table[i] = r
	}
	var crc uint32
	for _, b := range data {
		crc = (crc << 8) ^ table[byte(crc>>24)^b]
	}
	return crc
}

func testOggPage(headerType byte, sequence uint32, packets ...[]byte) []byte {
	var segments, payload []byte
	for _, packet := range packets {
		size := len(packet)
		for size >= 255 {
			segments = append(segments, 255)
			size -= 255
		}
		segments = append(segments, byte(size))
		payload = append(payload, packet...)
	}
	page := make([]byte, 27, 27+len(segments)+len(payload))
	copy(page, "OggS")
	page[5] = headerType
	binary.LittleEndian.PutUint32(page[14:], 1) // stream serial
	binary.LittleEndian.PutUint32(page[18:], sequence)
	page[26] = byte(len(segments))
	page = append(page, segments...)
	page = append(page, payload...)
	binary.LittleEndian.PutUint32(page[22:], testOggCRC(page))
	return page
}

// testOpusHeaders returns the OpusHead and OpusTags pages.
func testOpusHeaders() []byte {
	head := []byte("OpusHead")
	head = append(head, 1, 2)                            // version, channels
	head = binary.LittleEndian.AppendUint16(head, 312)   // pre-skip
	head = binary.LittleEndian.AppendUint32(head, 48000) // input sample rate
	head = append(head, 0, 0, 0)                         // output gain, mapping family
	tags := []byte("OpusTags")
	tags = binary.LittleEndian.AppendUint32(tags, 4)
	tags = append(tags, "test"...)
	tags = binary.LittleEndian.AppendUint32(tags, 0)
	return append(testOggPage(0x02, 0, head), testOggPage(0, 1, tags)...)
}

// testAudioPage holds count 2.5ms CELT packets (TOC config 28, one frame).
func testAudioPage(sequence uint32, count int) []byte {
	packets := make([][]byte, count)
	for i := range packets {
		packets[i] = []byte{28 << 3, byte(i), 0xAA}
	}
	return testOggPage(0, sequence, packets...)
}

const testAudioPacketDuration = 2500 * time.Microsecond

func appendBytes(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func appendFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := appendBytes(path, data); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

// readResult is one Read issued from a goroutine so a test can tell whether
// it is still blocked.
type readResult struct {
	data []byte
	err  error
}

func readAsync(reader io.Reader, size int) <-chan readResult {
	result := make(chan readResult, 1)
	go func() {
		buf := make([]byte, size)
		n, err := reader.Read(buf)
		result <- readResult{data: buf[:n], err: err}
	}()
	return result
}

func expectBlocked(t *testing.T, result <-chan readResult) {
	t.Helper()
	select {
	case got := <-result:
		t.Fatalf("read returned (%q, %v) while the transcode was still running", got.data, got.err)
	case <-time.After(5 * streamPollInterval):
	}
}

func expectRead(t *testing.T, result <-chan readResult) readResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("read did not return")
		return readResult{}
	}
}

func TestFollowReaderWaitsAtEOFUntilTranscodeFinishes(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "track.ogg.tmp")
	final := filepath.Join(dir, "track.ogg")
	appendFile(t, tmp, []byte("first"))
	stream := newMediaStream(tmp, final, "item")
	file, err := stream.open()
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer file.Close()
	reader := newFollowReader(context.Background(), file, stream)

	if got := expectRead(t, readAsync(reader, 64)); string(got.data) != "first" || got.err != nil {
		t.Fatalf("first read = (%q, %v), want first", got.data, got.err)
	}
	// At the end of what ffmpeg has written so far, Read must wait, not EOF.
	pending := readAsync(reader, 64)
	expectBlocked(t, pending)
	appendFile(t, tmp, []byte("second"))
	if got := expectRead(t, pending); string(got.data) != "second" || got.err != nil {
		t.Fatalf("read after append = (%q, %v), want second", got.data, got.err)
	}

	// Bytes written just before completion are still delivered, from the same
	// descriptor after the rename into the cache.
	appendFile(t, tmp, []byte("last"))
	if err := stream.publish(func() error { return os.Rename(tmp, final) }); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := expectRead(t, readAsync(reader, 64)); string(got.data) != "last" || got.err != nil {
		t.Fatalf("read after publish = (%q, %v), want last", got.data, got.err)
	}
	if got := expectRead(t, readAsync(reader, 64)); len(got.data) != 0 || !errors.Is(got.err, io.EOF) {
		t.Fatalf("read at end = (%q, %v), want io.EOF", got.data, got.err)
	}
}

func TestFollowReaderSurfacesTranscodeFailureAfterDrainingWrittenBytes(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "track.ogg.tmp")
	appendFile(t, tmp, []byte("partial"))
	stream := newMediaStream(tmp, filepath.Join(dir, "track.ogg"), "item")
	file, err := stream.open()
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer file.Close()
	reader := newFollowReader(context.Background(), file, stream)

	if got := expectRead(t, readAsync(reader, 64)); string(got.data) != "partial" {
		t.Fatalf("first read = (%q, %v), want partial", got.data, got.err)
	}
	pending := readAsync(reader, 64)
	expectBlocked(t, pending)
	transcodeErr := errors.New("ffmpeg: connection reset")
	stream.fail(transcodeErr)
	// The failed transcode removes its temporary file; the open descriptor
	// still reads the unlinked inode.
	_ = os.Remove(tmp)
	if got := expectRead(t, pending); !errors.Is(got.err, transcodeErr) {
		t.Fatalf("read after failure = (%q, %v), want %v", got.data, got.err, transcodeErr)
	}
	if _, err := stream.open(); !errors.Is(err, transcodeErr) {
		t.Fatalf("open after failure error = %v, want %v", err, transcodeErr)
	}
}

func TestFollowReaderStopsWaitingWhenCancelled(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "track.ogg.tmp")
	appendFile(t, tmp, nil)
	stream := newMediaStream(tmp, filepath.Join(dir, "track.ogg"), "item")
	file, err := stream.open()
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer file.Close()
	ctx, cancel := context.WithCancel(context.Background())
	pending := readAsync(newFollowReader(ctx, file, stream), 64)
	expectBlocked(t, pending)
	cancel()
	if got := expectRead(t, pending); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("read after cancel error = %v, want context.Canceled", got.err)
	}
}

func TestCompleteOggPagesCountsOnlyFullyWrittenPages(t *testing.T) {
	data := append(testOpusHeaders(), testAudioPage(2, 4)...)
	if got := completeOggPages(bytes.NewReader(data), streamPlayablePages); got != 3 {
		t.Fatalf("complete pages = %d, want 3", got)
	}
	for _, cut := range []int{1, 20, 40} {
		truncated := data[:len(data)-cut]
		if got := completeOggPages(bytes.NewReader(truncated), streamPlayablePages); got != 2 {
			t.Fatalf("complete pages with %d bytes missing = %d, want 2", cut, got)
		}
	}
	if got := completeOggPages(bytes.NewReader([]byte("ID3 not ogg at all......................")), 3); got != 0 {
		t.Fatalf("complete pages of non-Ogg data = %d, want 0", got)
	}

	path := filepath.Join(t.TempDir(), "growing.ogg.tmp")
	if oggFilePlayable(path) {
		t.Fatal("missing file reported playable")
	}
	appendFile(t, path, testOpusHeaders())
	if oggFilePlayable(path) {
		t.Fatal("headers without audio reported playable")
	}
	appendFile(t, path, testAudioPage(2, 4))
	if !oggFilePlayable(path) {
		t.Fatal("headers plus a complete audio page not reported playable")
	}
}

func newStreamingTestManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{
		cfg:         Config{Dir: t.TempDir(), OpusBitrate: "128k"},
		cacheLeases: map[string]int{},
		streams:     map[string]*mediaStream{},
	}
}

// startTestStream registers an in-flight transcode for item the way
// prepareBroadcastMedia does, with tmp holding data.
func startTestStream(t *testing.T, m *Manager, item *QueueItem, data []byte) *mediaStream {
	t.Helper()
	final := m.broadcastCachePath(item)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	tmp := final + ".tmp-test"
	appendFile(t, tmp, data)
	return m.beginStream(tmp, final, item.ID)
}

func TestItemPlayableTreatsOnlyReadyAndStreamingRowsAsPlayable(t *testing.T) {
	m := newStreamingTestManager(t)
	row := func(id string, status QueueStatus) *QueueItem {
		return &QueueItem{ID: id, RoomID: "r1", Source: "netease", TrackID: "track-" + id, Status: status}
	}
	ready := row("ready", StatusReady)
	pending := row("pending", StatusPending)
	failed := row("failed", StatusFailed)
	downloading := row("downloading", StatusDownloading)
	for _, item := range []*QueueItem{pending, failed} {
		// Even a playable stream for the same track doesn't make these rows
		// playable: only a row this room is downloading may stream.
		startTestStream(t, m, item, nil).markPlayable()
	}

	if !m.ItemPlayable(ready) {
		t.Fatal("ready row is not playable")
	}
	for _, item := range []*QueueItem{pending, failed, nil} {
		if m.ItemPlayable(item) {
			t.Fatalf("row %+v is playable", item)
		}
	}
	if m.ItemPlayable(downloading) {
		t.Fatal("downloading row without a stream is playable")
	}
	stream := startTestStream(t, m, downloading, testOpusHeaders())
	if m.ItemPlayable(downloading) {
		t.Fatal("downloading row is playable before its first audio page")
	}
	stream.markPlayable()
	if !m.ItemPlayable(downloading) {
		t.Fatal("streaming row is not playable")
	}
	// A row in another room downloading the same track shares the stream.
	otherRoom := &QueueItem{ID: "other", RoomID: "r2", Source: "netease", TrackID: downloading.TrackID, Status: StatusDownloading}
	if !m.ItemPlayable(otherRoom) {
		t.Fatal("same track downloading in another room is not playable")
	}

	stream.fail(errors.New("upstream 403"))
	if m.ItemPlayable(downloading) {
		t.Fatal("row is still playable after its transcode failed")
	}
}

func TestRetireStreamKeepsOtherOwnersAndRunningStreams(t *testing.T) {
	m := newStreamingTestManager(t)
	item := &QueueItem{ID: "owner", RoomID: "r1", Source: "netease", TrackID: "t1", Status: StatusDownloading}
	final := m.broadcastCachePath(item)
	stream := startTestStream(t, m, item, nil)

	m.retireStream(final, item.ID)
	if m.streamFor(final) != stream {
		t.Fatal("a running stream was retired")
	}
	if err := stream.publish(func() error { return nil }); err != nil {
		t.Fatalf("publish: %v", err)
	}
	m.retireStream(final, "waiting-row-in-another-room")
	if m.streamFor(final) != stream {
		t.Fatal("a stream was retired by a row that doesn't own it")
	}
	m.retireStream(final, item.ID)
	if m.streamFor(final) != nil {
		t.Fatal("finished stream was not retired by its owner")
	}
}

func TestOpenBroadcastMediaFollowsStreamThroughPublishAndHoldsLease(t *testing.T) {
	m := newStreamingTestManager(t)
	item := &QueueItem{ID: "item", RoomID: "r1", Source: "netease", TrackID: "t1", Status: StatusDownloading}
	first := testOpusHeaders()
	stream := startTestStream(t, m, item, first)
	stream.markPlayable()
	final := m.broadcastCachePath(item)

	media, err := m.openBroadcastMedia(context.Background(), item)
	if err != nil {
		t.Fatalf("open streaming media: %v", err)
	}
	if !m.mediaPathLeased(final) {
		t.Fatal("streaming reader holds no lease on the final cache path")
	}
	rest := testAudioPage(2, 4)
	go func() {
		time.Sleep(3 * streamPollInterval)
		if err := appendBytes(stream.tmpPath, rest); err != nil {
			t.Errorf("append: %v", err)
		}
		if err := stream.publish(func() error { return os.Rename(stream.tmpPath, final) }); err != nil {
			t.Errorf("publish: %v", err)
		}
	}()
	got, err := io.ReadAll(media)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if want := append(append([]byte(nil), first...), rest...); !bytes.Equal(got, want) {
		t.Fatalf("streamed %d bytes, want the %d bytes written across publish", len(got), len(want))
	}
	if err := media.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if m.mediaPathLeased(final) {
		t.Fatal("lease outlived the reader")
	}

	// Once published (and before the row is marked ready), new readers open
	// the cache file itself.
	again, err := m.openBroadcastMedia(context.Background(), item)
	if err != nil {
		t.Fatalf("open published stream: %v", err)
	}
	defer again.Close()
	if got, err := io.ReadAll(again); err != nil || len(got) != len(first)+len(rest) {
		t.Fatalf("published read = (%d bytes, %v), want %d bytes", len(got), err, len(first)+len(rest))
	}
}

func TestEstimateDurationMS(t *testing.T) {
	cases := []struct {
		sizeKB  int64
		bitrate int
		want    int64
	}{
		{sizeKB: 9840, bitrate: 320, want: 251904}, // a ~4:12 320k MP3
		{sizeKB: 4096, bitrate: 128, want: 262144},
		{sizeKB: 0, bitrate: 320, want: 0},
		{sizeKB: 4096, bitrate: 0, want: 0},
		{sizeKB: -1, bitrate: 320, want: 0},
	}
	for _, tc := range cases {
		if got := estimateDurationMS(tc.sizeKB, tc.bitrate); got != tc.want {
			t.Errorf("estimateDurationMS(%d, %d) = %d, want %d", tc.sizeKB, tc.bitrate, got, tc.want)
		}
	}
}
