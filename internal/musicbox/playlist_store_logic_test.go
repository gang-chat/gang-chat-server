package musicbox

import "testing"

func TestNormalizePlaylistPage(t *testing.T) {
	page, pageSize := normalizePage(0, 0)
	if page != 1 || pageSize != DefaultPlaylistPage {
		t.Fatalf("normalizePage(0, 0) = (%d, %d)", page, pageSize)
	}
	page, pageSize = normalizePage(3, MaxPlaylistPage+100)
	if page != 3 || pageSize != MaxPlaylistPage {
		t.Fatalf("normalizePage cap = (%d, %d)", page, pageSize)
	}
}

func TestSameStringSetRejectsDuplicatesAndStaleOrders(t *testing.T) {
	if !sameStringSet(
		[]string{"item_1", "item_2"},
		[]string{"item_2", "item_1"},
	) {
		t.Fatal("same item ids in another order should be accepted")
	}
	if sameStringSet(
		[]string{"item_1", "item_2"},
		[]string{"item_1", "item_1"},
	) {
		t.Fatal("duplicate request ids should be rejected")
	}
	if sameStringSet([]string{"item_1"}, []string{"item_1", "item_2"}) {
		t.Fatal("stale partial order should be rejected")
	}
}

func TestClonePlaylistNameAndArtistsStayDisplaySafe(t *testing.T) {
	// Use visible runes so the test also catches byte-based truncation of CJK.
	longName := "朋友的歌单 · " + repeatRune('歌', 80)
	trimmed := truncatePlaylistName(longName, 64)
	if got := len([]rune(trimmed)); got != 64 {
		t.Fatalf("truncated clone name has %d runes, want 64", got)
	}
	artists := splitSnapshotArtists("甲、乙, 丙，丁")
	want := []string{"甲", "乙", "丙", "丁"}
	if !sameStringList(artists, want) {
		t.Fatalf("splitSnapshotArtists() = %v, want %v", artists, want)
	}
}

func repeatRune(value rune, count int) string {
	runes := make([]rune, count)
	for index := range runes {
		runes[index] = value
	}
	return string(runes)
}
