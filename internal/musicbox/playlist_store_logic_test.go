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

func TestSelectMergedPlaylistTracksPreservesOrderAndDeduplicatesByLink(t *testing.T) {
	sources := []playlistMergeSource{
		{
			playlistID: "playlist-1",
			tracks: []playlistMergeTrack{
				{itemID: "item-a", trackID: "track-a", source: "netease", externalTrackID: "link-1"},
				{itemID: "item-b", trackID: "track-b", source: "netease", externalTrackID: "link-2"},
				{itemID: "item-a-2", trackID: "track-a", source: "netease", externalTrackID: "link-1"},
			},
		},
		{
			playlistID: "playlist-2",
			tracks: []playlistMergeTrack{
				{itemID: "item-c", trackID: "track-c", source: "netease", externalTrackID: "link-2"},
				{itemID: "item-d", trackID: "track-d", source: "bilibili", externalTrackID: "link-2"},
				{itemID: "item-e", trackID: "track-e", source: "netease", externalTrackID: "link-3"},
			},
		},
	}
	result := selectMergedPlaylistTracks(sources, 3)
	wantTrackIDs := []string{"track-a", "track-b", "track-d"}
	if !sameStringList(result.targetTrackIDs, wantTrackIDs) {
		t.Fatalf("merged track order = %v, want %v", result.targetTrackIDs, wantTrackIDs)
	}
	if result.sourceItemCount != 6 || result.uniqueItemCount != 4 {
		t.Fatalf(
			"merge counts = source %d unique %d, want source 6 unique 4",
			result.sourceItemCount,
			result.uniqueItemCount,
		)
	}
	if !sameStringList(result.deletedPlaylistIDs, []string{"playlist-1"}) {
		t.Fatalf("deleted sources = %v, want playlist-1", result.deletedPlaylistIDs)
	}
	if result.consumedSourceItemCount != 5 ||
		!sameStringList(
			result.consumedItemIDs["playlist-2"],
			[]string{"item-c", "item-d"},
		) {
		t.Fatalf("unexpected consumed prefixes: %+v", result)
	}
}

func TestNormalizedMergePlaylistIDsRequiresTwoUniqueValues(t *testing.T) {
	if got, ok := normalizedMergePlaylistIDs([]string{" first ", "second"}); !ok || !sameStringList(got, []string{"first", "second"}) {
		t.Fatalf("normalized ids = %v, %v", got, ok)
	}
	for _, invalid := range [][]string{
		{"only-one"},
		{"same", "same"},
		{"first", " "},
	} {
		if got, ok := normalizedMergePlaylistIDs(invalid); ok || got != nil {
			t.Fatalf("invalid merge ids %v accepted as %v", invalid, got)
		}
	}
}

func repeatRune(value rune, count int) string {
	runes := make([]rune, count)
	for index := range runes {
		runes[index] = value
	}
	return string(runes)
}
