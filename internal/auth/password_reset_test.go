package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
)

func TestPasswordResetHelpers(t *testing.T) {
	if got := maskEmail("kai@example.com"); got != "ki***@example.com" {
		t.Fatalf("maskEmail: got %q", got)
	}
	if got := maskEmail("a@example.com"); got != "a***@example.com" {
		t.Fatalf("mask short email: got %q", got)
	}
	for range 20 {
		code, err := randomNumericCode()
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`^[0-9]{6}$`).MatchString(code) {
			t.Fatalf("invalid code %q", code)
		}
	}
	if hashResetToken("token") == hashResetToken("other") {
		t.Fatal("different reset tokens must not hash equally")
	}
}

func TestResendPasswordResetEmailSender(t *testing.T) {
	var authorization string
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		if r.URL.Path != "/emails" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"email-1"}`))
	}))
	defer server.Close()

	sender := newPasswordResetEmailSender(&config.Config{
		ResendAPIBaseURL: server.URL,
		ResendAPIKey:     "resend-key",
		EmailFrom:        "Gang Chat <no-reply@example.com>",
	})
	if sender == nil {
		t.Fatal("configured sender should be available")
	}
	if err := sender.SendPasswordResetCode(context.Background(), "kai@example.com", "123456"); err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer resend-key" {
		t.Fatalf("authorization = %q", authorization)
	}
	if payload["from"] != "Gang Chat <no-reply@example.com>" || payload["subject"] != "Gang Chat 密码重置验证码" {
		t.Fatalf("unexpected payload: %v", payload)
	}
}
