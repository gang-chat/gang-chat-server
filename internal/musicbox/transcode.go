package musicbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// transcoder downloads a source URL and transcodes it to an Ogg/Opus file via
// ffmpeg, streaming so memory stays bounded regardless of track length. A
// fixed-size worker pool caps concurrent ffmpeg processes so a burst of
// enqueues can't exhaust memory/CPU.
type transcoder struct {
	ffmpegPath  string
	opusBitrate string
	slots       chan struct{}
}

func newTranscoder(ffmpegPath, opusBitrate string, workers int) *transcoder {
	if workers <= 0 {
		workers = 3
	}
	return &transcoder{
		ffmpegPath:  ffmpegPath,
		opusBitrate: opusBitrate,
		slots:       make(chan struct{}, workers),
	}
}

// transcodeResult reports the produced file's size and measured duration.
type transcodeResult struct {
	SizeBytes  int64
	DurationMS int64
}

// transcode reads sourceURL (an http(s) URL or local path) and writes an
// Ogg/Opus file to dstPath. It blocks on a worker slot first, so callers run
// it from their own goroutine. ctx cancellation kills the ffmpeg process.
//
// source is the GD music source the URL came from; some sources hand back a
// CDN URL that enforces hotlink protection (bilibili's *.bilivideo.com checks
// Referer), so we set the matching request headers for those.
func (t *transcoder) transcode(ctx context.Context, source, sourceURL, dstPath string) (*transcodeResult, error) {
	select {
	case t.slots <- struct{}{}:
		defer func() { <-t.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// ffmpeg reads the (possibly remote) input and produces Opus in an Ogg
	// container — the exact format LiveKit's reader track consumes. -vn drops
	// any embedded cover art video stream. 48kHz stereo matches WebRTC Opus.
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
	}
	// Hotlink-protected sources need browser-like request headers on the input,
	// or the CDN answers 403. These ffmpeg flags must precede the matching -i.
	args = append(args, inputHeaderArgs(source)...)
	args = append(args,
		"-i", sourceURL,
		"-vn",
		"-c:a", "libopus",
		"-b:a", t.opusBitrate,
		// In-band FEC embeds a low-bitrate copy of the previous frame in each
		// packet so listeners recover isolated packet loss without a retransmit
		// — the main stability win for the broadcast. packet_loss tells the
		// encoder how aggressively to protect; ~10% is the sweet spot (audible
		// loss recovery, negligible bitrate/quality cost when the network is fine).
		"-fec", "1",
		"-packet_loss", "10",
		"-ar", "48000",
		"-ac", "2",
		"-f", "ogg",
		dstPath,
	)
	cmd := exec.CommandContext(ctx, t.ffmpegPath, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(dstPath)
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("ffmpeg: %s", truncateErr(msg))
	}

	info, err := os.Stat(dstPath)
	if err != nil {
		return nil, fmt.Errorf("stat output: %w", err)
	}
	if info.Size() == 0 {
		_ = os.Remove(dstPath)
		return nil, fmt.Errorf("ffmpeg produced empty output")
	}

	dur := t.probeDuration(ctx, dstPath)
	return &transcodeResult{SizeBytes: info.Size(), DurationMS: dur}, nil
}

// probeDuration asks ffprobe for the output duration; best-effort (0 on
// failure). ffprobe ships with ffmpeg. We derive its path from ffmpegPath so a
// custom install location still works.
func (t *transcoder) probeDuration(ctx context.Context, path string) int64 {
	probe := ffprobePath(t.ffmpegPath)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, probe,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	secs := strings.TrimSpace(string(out))
	var f float64
	if _, err := fmt.Sscanf(secs, "%f", &f); err != nil || f <= 0 {
		return 0
	}
	return int64(f * 1000)
}

// inputHeaderArgs returns the ffmpeg input flags needed to fetch a given
// source's CDN URL. bilibili serves audio from *.bilivideo.com, which rejects
// requests without a bilibili Referer (HTTP 403); a browser-like User-Agent
// and Referer satisfy the hotlink check. Other sources need nothing.
func inputHeaderArgs(source string) []string {
	if source != "bilibili" {
		return nil
	}
	return []string{
		"-user_agent", "Mozilla/5.0",
		// ffmpeg expects raw CRLF-terminated header lines here.
		"-headers", "Referer: https://www.bilibili.com\r\n",
	}
}

func ffprobePath(ffmpeg string) string {
	if i := strings.LastIndex(ffmpeg, "ffmpeg"); i >= 0 {
		return ffmpeg[:i] + "ffprobe" + ffmpeg[i+len("ffmpeg"):]
	}
	return "ffprobe"
}

func truncateErr(s string) string {
	const max = 500
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}
