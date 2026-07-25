package push

import (
	"strings"
	"testing"
)

func TestMessagePreview(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		messageType string
		body        string
		want        string
	}{
		{name: "text", messageType: "text", body: "  hello  ", want: "hello"},
		{name: "sticker", messageType: "sticker", want: "[表情]"},
		{name: "audio", messageType: "audio", want: "[语音]"},
		{name: "file", messageType: "file", want: "[文件]"},
		{name: "empty text", messageType: "text", want: "收到一条新消息"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := messagePreview(test.messageType, test.body); got != test.want {
				t.Fatalf("messagePreview() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMessagePreviewLimitsUnicodeByRune(t *testing.T) {
	t.Parallel()
	got := messagePreview("text", strings.Repeat("你", 181))
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("messagePreview() = %q, want ellipsis", got)
	}
	if count := len([]rune(got)); count != 180 {
		t.Fatalf("messagePreview() rune count = %d, want 180", count)
	}
}
