package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
)

type PasswordResetEmailSender interface {
	SendPasswordResetCode(ctx context.Context, to, code string) error
}

type resendPasswordResetEmailSender struct {
	apiBaseURL string
	apiKey     string
	from       string
	httpClient *http.Client
}

func newPasswordResetEmailSender(cfg *config.Config) PasswordResetEmailSender {
	if strings.TrimSpace(cfg.ResendAPIKey) == "" || strings.TrimSpace(cfg.EmailFrom) == "" {
		return nil
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.ResendAPIBaseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.resend.com"
	}
	return &resendPasswordResetEmailSender{
		apiBaseURL: baseURL,
		apiKey:     strings.TrimSpace(cfg.ResendAPIKey),
		from:       strings.TrimSpace(cfg.EmailFrom),
		httpClient: &http.Client{Timeout: 12 * time.Second},
	}
}

func (s *resendPasswordResetEmailSender) SendPasswordResetCode(ctx context.Context, to, code string) error {
	escapedCode := html.EscapeString(code)
	payload, err := json.Marshal(map[string]any{
		"from":    s.from,
		"to":      []string{to},
		"subject": "Gang Chat 密码重置验证码",
		"text":    fmt.Sprintf("您的 Gang Chat 密码重置验证码是 %s。验证码 10 分钟内有效。如果不是您本人操作，请忽略此邮件。", code),
		"html": fmt.Sprintf(
			`<div style="font-family:Arial,sans-serif;color:#20242c"><h2>重置 Gang Chat 密码</h2><p>您的验证码是：</p><p style="font-size:28px;font-weight:700;letter-spacing:6px">%s</p><p>验证码 10 分钟内有效。如果不是您本人操作，请忽略此邮件。</p></div>`,
			escapedCode,
		),
	})
	if err != nil {
		return fmt.Errorf("encode Resend request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiBaseURL+"/emails", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create Resend request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send password-reset email: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("Resend rejected password-reset email: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
}
