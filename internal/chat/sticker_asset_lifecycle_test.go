package chat

import "testing"

func TestStickerAssetFromAttachmentsUsesImmutableMessageSnapshot(t *testing.T) {
	attachments := []any{
		map[string]any{
			"type":       "sticker",
			"sticker_id": "stk_1",
			"name":       "hello",
			"asset": map[string]any{
				"id":  "ast_1",
				"url": "/assets/ast_1/hello.webp",
			},
		},
	}

	assetID, name, ok := stickerAssetFromAttachments(attachments, "stk_1")
	if !ok || assetID != "ast_1" || name != "hello" {
		t.Fatalf("unexpected snapshot result: asset=%q name=%q ok=%v", assetID, name, ok)
	}
	if _, _, ok := stickerAssetFromAttachments(attachments, "stk_missing"); ok {
		t.Fatal("a different sticker id must not reuse the message asset")
	}
}

func TestStickerAssetIDsFromAttachmentsKeepsStickerAssetsUnique(t *testing.T) {
	attachments := []any{
		map[string]any{"type": "sticker", "asset": map[string]any{"id": "ast_1"}},
		map[string]any{"type": "file", "asset": map[string]any{"id": "ast_file"}},
		map[string]any{"type": "sticker", "asset": map[string]any{"id": "ast_1"}},
		map[string]any{"type": "sticker", "asset": map[string]any{"id": "ast_2"}},
	}

	got := stickerAssetIDsFromAttachments(attachments)
	if len(got) != 2 || got[0] != "ast_1" || got[1] != "ast_2" {
		t.Fatalf("unexpected sticker asset ids: %v", got)
	}
}
