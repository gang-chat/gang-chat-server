package chat

import "testing"

func TestMessageMatchesHistoryCategory(t *testing.T) {
	t.Parallel()

	asset := func(filename, mimeType string) map[string]any {
		return map[string]any{
			"id":        "asset-1",
			"filename":  filename,
			"mime_type": mimeType,
		}
	}
	attachment := func(attachmentType, filename, mimeType string) map[string]any {
		return map[string]any{
			"type":      attachmentType,
			"filename":  filename,
			"mime_type": mimeType,
			"asset":     asset(filename, mimeType),
		}
	}

	tests := []struct {
		name     string
		message  message
		category string
		want     bool
	}{
		{
			name:     "top-level image mime",
			message:  message{Type: "text", Attachments: []any{attachment("file", "photo.png", "image/png")}},
			category: "images",
			want:     true,
		},
		{
			name: "nested image mime",
			message: message{Type: "file", Attachments: []any{map[string]any{
				"type":  "file",
				"asset": asset("photo.webp", "image/webp"),
			}}},
			category: "images",
			want:     true,
		},
		{
			name:     "image is not an ordinary file",
			message:  message{Type: "file", Attachments: []any{attachment("file", "photo.jpg", "image/jpeg")}},
			category: "files",
			want:     false,
		},
		{
			name:     "pdf file",
			message:  message{Type: "file", Attachments: []any{attachment("file", "notes.pdf", "application/pdf")}},
			category: "files",
			want:     true,
		},
		{
			name: "nested pdf mime",
			message: message{Type: "file", Attachments: []any{map[string]any{
				"type":  "file",
				"asset": asset("notes.pdf", "application/pdf"),
			}}},
			category: "files",
			want:     true,
		},
		{
			name:     "ordinary audio upload remains a file",
			message:  message{Type: "file", Attachments: []any{attachment("file", "music.mp3", "audio/mpeg")}},
			category: "files",
			want:     true,
		},
		{
			name: "legacy voice file",
			message: message{Type: "file", Attachments: []any{map[string]any{
				"type":  "file",
				"name":  `recordings\voice_123.m4a`,
				"asset": asset("voice_123.m4a", "audio/mp4"),
			}}},
			category: "voice",
			want:     true,
		},
		{
			name: "legacy voice file excluded from files",
			message: message{Type: "file", Attachments: []any{map[string]any{
				"type":  "file",
				"name":  "voice_123.m4a",
				"asset": asset("voice_123.m4a", "audio/mp4"),
			}}},
			category: "files",
			want:     false,
		},
		{
			name: "current voice attachment",
			message: message{Type: "audio", Attachments: []any{map[string]any{
				"type":        "audio",
				"duration_ms": float64(1200),
			}}},
			category: "voice",
			want:     true,
		},
		{
			name:     "link text",
			message:  message{Type: "text", Body: "See HTTPS://example.com/a"},
			category: "links",
			want:     true,
		},
		{
			name:     "link-looking file body is not a link message",
			message:  message{Type: "file", Body: "https://example.com/a"},
			category: "links",
			want:     false,
		},
		{
			name:     "sticker attachment",
			message:  message{Type: "text", Attachments: []any{map[string]any{"type": "sticker"}}},
			category: "stickers",
			want:     true,
		},
		{
			name:     "system message",
			message:  message{Type: "system"},
			category: "system",
			want:     true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := messageMatchesHistoryCategory(test.message, test.category); got != test.want {
				t.Fatalf("messageMatchesHistoryCategory(%q) = %v, want %v", test.category, got, test.want)
			}
		})
	}
}

func TestValidMessageHistoryCategory(t *testing.T) {
	t.Parallel()
	for _, category := range []string{"all", "links", "voice", "stickers", "images", "files", "system"} {
		if !validMessageHistoryCategory(category) {
			t.Fatalf("expected %q to be valid", category)
		}
	}
	if validMessageHistoryCategory("video") {
		t.Fatal("unexpected valid category: video")
	}
}
