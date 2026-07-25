package push

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewFCMSenderDisabledWithoutServiceAccount(t *testing.T) {
	t.Parallel()
	sender, err := NewFCMSender("", "")
	if err != nil {
		t.Fatalf("NewFCMSender() error = %v", err)
	}
	if sender != nil {
		t.Fatal("NewFCMSender() should be nil when push is not configured")
	}
}

func TestNewFCMSenderRejectsIncompleteServiceAccount(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "service-account.json")
	if err := os.WriteFile(path, []byte(`{"project_id":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFCMSender("", path); err == nil {
		t.Fatal("NewFCMSender() accepted an incomplete service account")
	}
}
