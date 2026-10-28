package chat

import (
	"errors"
	"testing"
)

func TestNumberedNameChangesOnlyTheNewDuplicate(t *testing.T) {
	tests := []struct {
		occurrence int
		want       string
	}{
		{occurrence: 1, want: "same"},
		{occurrence: 2, want: "same (2)"},
		{occurrence: 3, want: "same (3)"},
	}
	for _, test := range tests {
		if got := numberedName("same", test.occurrence); got != test.want {
			t.Fatalf("numberedName occurrence %d = %q, want %q", test.occurrence, got, test.want)
		}
	}
}

func TestStickerNameConflictRecognizesUniqueViolation(t *testing.T) {
	pool := openTestDatabase(t)
	if _, err := pool.Exec(`INSERT INTO id_sequences (name, next_value) VALUES ('dup', 1)`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	_, err := pool.Exec(`INSERT INTO id_sequences (name, next_value) VALUES ('dup', 2)`)
	if !isStickerNameConflict(err) {
		t.Fatalf("unique-constraint errors must be retried with a numbered name: %v", err)
	}
	if isStickerNameConflict(errors.New("connection closed")) {
		t.Fatal("non-conflict database errors must not be swallowed")
	}
}

func TestPrependedStickerSortOrderKeepsExistingRelativeOrder(t *testing.T) {
	if got := prependedStickerSortOrder(0, 0); got != 10 {
		t.Fatalf("first sticker sort order = %d, want 10", got)
	}
	if got := prependedStickerSortOrder(3, -10); got != -20 {
		t.Fatalf("prepended sticker sort order = %d, want -20", got)
	}
}
