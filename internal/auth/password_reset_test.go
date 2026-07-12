package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
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

func TestResendVerificationEmailSender(t *testing.T) {
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

	sender := newVerificationEmailSender(&config.Config{
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
	if payload["from"] != "Gang Chat <no-reply@example.com>" || payload["subject"] != "Gang Chat｜密码重置验证码" {
		t.Fatalf("unexpected payload: %v", payload)
	}
	htmlBody, _ := payload["html"].(string)
	if !strings.Contains(htmlBody, "cid:gang-chat-logo") ||
		!strings.Contains(htmlBody, "#181c24") ||
		!strings.Contains(htmlBody, "123456") {
		t.Fatalf("email HTML does not contain the branded template: %s", htmlBody)
	}
	attachments, _ := payload["attachments"].([]any)
	if len(attachments) != 1 {
		t.Fatalf("want one inline logo attachment, got %v", payload["attachments"])
	}
	logo, _ := attachments[0].(map[string]any)
	if logo["content_id"] != "gang-chat-logo" || logo["content_type"] != "image/png" || logo["content"] == "" {
		t.Fatalf("unexpected inline logo: %v", logo)
	}
	logoBytes, err := base64.StdEncoding.DecodeString(logo["content"].(string))
	if err != nil || !bytes.HasPrefix(logoBytes, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("inline logo is not a valid PNG: %v", err)
	}

	if err := sender.SendRegistrationVerificationCode(context.Background(), "kai@example.com", "654321"); err != nil {
		t.Fatal(err)
	}
	if payload["subject"] != "Gang Chat｜邮箱验证码" {
		t.Fatalf("unexpected registration subject: %v", payload["subject"])
	}
	htmlBody, _ = payload["html"].(string)
	if !strings.Contains(htmlBody, "验证您的邮箱") || !strings.Contains(htmlBody, "654321") {
		t.Fatalf("registration email HTML is incorrect: %s", htmlBody)
	}
}
