package chat

import (
	"errors"
	"testing"

	mysqlDriver "github.com/go-sql-driver/mysql"
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

func TestStickerNameConflictRecognizesMySQLDuplicate(t *testing.T) {
	err := &mysqlDriver.MySQLError{Number: 1062, Message: "Duplicate entry"}
	if !isStickerNameConflict(err) {
		t.Fatal("MySQL duplicate-key errors must be retried with a numbered name")
	}
	if isStickerNameConflict(errors.New("connection closed")) {
		t.Fatal("non-conflict database errors must not be swallowed")
	}
}
