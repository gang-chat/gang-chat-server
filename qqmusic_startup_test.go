package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInitOptionalQQMusicDisabledWhenUnconfigured(t *testing.T) {
	if got := initOptionalQQMusic("", "secret", time.Second, t.Logf); got != nil {
		t.Fatal("expected an unconfigured QQ音乐 source to stay disabled")
	}
	if got := initOptionalQQMusic("http://example.invalid", "", time.Second, t.Logf); got != nil {
		t.Fatal("expected a QQ音乐 source without a password to stay disabled")
	}
}

func TestInitOptionalQQMusicReturnsClientAfterSuccessfulProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/login" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if got := initOptionalQQMusic(server.URL, "secret", time.Second, t.Logf); got == nil {
		t.Fatal("expected a client after a successful startup probe")
	}
}

func TestInitOptionalQQMusicDegradesWhenServiceIsUnavailable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve unavailable address: %v", err)
	}
	baseURL := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close reserved listener: %v", err)
	}

	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	if got := initOptionalQQMusic(baseURL, "secret", time.Second, logf); got != nil {
		t.Fatal("expected an unavailable optional source to be disabled")
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "source disabled") {
		t.Fatalf("expected one degradation log, got %q", logs)
	}
}
