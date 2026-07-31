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
