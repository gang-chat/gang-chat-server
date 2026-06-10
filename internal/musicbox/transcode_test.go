package musicbox

import "testing"

func TestFFprobePath(t *testing.T) {
	cases := map[string]string{
		"ffmpeg":                      "ffprobe",
		"/usr/bin/ffmpeg":             "/usr/bin/ffprobe",
		"/opt/homebrew/bin/ffmpeg":    "/opt/homebrew/bin/ffprobe",
		"C:\\tools\\ffmpeg.exe":       "C:\\tools\\ffprobe.exe",
		"/custom/path/no-match-here": "ffprobe",
	}
	for in, want := range cases {
		if got := ffprobePath(in); got != want {
			t.Errorf("ffprobePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeRoomID(t *testing.T) {
	cases := map[string]string{
		"room_123":   "room_123",
		"abc-DEF":    "abc-DEF",
		"../etc":     "___etc",
		"a/b\\c":     "a_b_c",
		"r 1":        "r_1",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
