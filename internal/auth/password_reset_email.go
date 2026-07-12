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

type VerificationEmailSender interface {
	SendPasswordResetCode(ctx context.Context, to, code string) error
	SendRegistrationVerificationCode(ctx context.Context, to, code string) error
}

type resendVerificationEmailSender struct {
	apiBaseURL string
	apiKey     string
	from       string
	httpClient *http.Client
}

func newVerificationEmailSender(cfg *config.Config) VerificationEmailSender {
	if strings.TrimSpace(cfg.ResendAPIKey) == "" || strings.TrimSpace(cfg.EmailFrom) == "" {
		return nil
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.ResendAPIBaseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.resend.com"
	}
	return &resendVerificationEmailSender{
		apiBaseURL: baseURL,
		apiKey:     strings.TrimSpace(cfg.ResendAPIKey),
		from:       strings.TrimSpace(cfg.EmailFrom),
		httpClient: &http.Client{Timeout: 12 * time.Second},
	}
}

func (s *resendVerificationEmailSender) SendPasswordResetCode(ctx context.Context, to, code string) error {
	return s.sendCode(ctx, to, code, verificationEmailContent{
		subject:     "Gang Chat｜密码重置验证码",
		plainText:   fmt.Sprintf("您的 Gang Chat 密码重置验证码是 %s。验证码 10 分钟内有效。如果不是您本人操作，请忽略此邮件。", code),
		preview:     "您的 Gang Chat 密码重置验证码是 " + code,
		title:       "重置您的密码",
		description: "请在 Gang Chat 的邮箱验证窗口中输入以下验证码",
		footer:      "如果不是您本人发起的密码重置，可以忽略此邮件，您的密码不会被更改。",
	})
}

func (s *resendVerificationEmailSender) SendRegistrationVerificationCode(ctx context.Context, to, code string) error {
	return s.sendCode(ctx, to, code, verificationEmailContent{
		subject:     "Gang Chat｜邮箱验证码",
		plainText:   fmt.Sprintf("您的 Gang Chat 邮箱验证码是 %s。验证码 10 分钟内有效。如果不是您本人操作，请忽略此邮件。", code),
		preview:     "您的 Gang Chat 邮箱验证码是 " + code,
		title:       "验证您的邮箱",
		description: "请在 Gang Chat 的注册邮箱验证窗口中输入以下验证码",
		footer:      "如果不是您本人发起的账号注册，可以忽略此邮件。",
	})
}

type verificationEmailContent struct {
	subject     string
	plainText   string
	preview     string
	title       string
	description string
	footer      string
}

func (s *resendVerificationEmailSender) sendCode(ctx context.Context, to, code string, content verificationEmailContent) error {
	payload, err := json.Marshal(map[string]any{
		"from":    s.from,
		"to":      []string{to},
		"subject": content.subject,
		"text":    content.plainText,
		"html":    verificationEmailHTML(code, content),
		"attachments": []map[string]string{
			{
				"content":      passwordResetLogoBase64,
				"filename":     "gang-chat.png",
				"content_id":   "gang-chat-logo",
				"content_type": "image/png",
			},
		},
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
		return fmt.Errorf("send verification email: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("Resend rejected verification email: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
}

func verificationEmailHTML(code string, content verificationEmailContent) string {
	escapedCode := html.EscapeString(code)
	escapedPreview := html.EscapeString(content.preview)
	escapedTitle := html.EscapeString(content.title)
	escapedDescription := html.EscapeString(content.description)
	escapedFooter := html.EscapeString(content.footer)
	return fmt.Sprintf(`<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="margin:0;padding:0;background:#101319;color:#eceff1;font-family:'Segoe UI','Microsoft YaHei',Arial,sans-serif;">
  <div style="display:none;max-height:0;overflow:hidden;color:transparent;">%s</div>
  <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="background:#101319;padding:32px 16px;">
    <tr><td align="center">
      <table role="presentation" width="100%%" cellspacing="0" cellpadding="0" border="0" style="max-width:520px;background:#181c24;border:1px solid #2a2f38;border-radius:16px;overflow:hidden;box-shadow:0 18px 42px rgba(0,0,0,.32);">
        <tr><td style="padding:26px 30px 22px;border-bottom:1px solid #2a2f38;">
          <table role="presentation" cellspacing="0" cellpadding="0" border="0"><tr>
            <td style="padding-right:13px;vertical-align:middle;"><img src="cid:gang-chat-logo" width="44" height="44" alt="Gang Chat" style="display:block;width:44px;height:44px;border:0;"></td>
            <td style="vertical-align:middle;"><div style="font-size:22px;line-height:28px;font-weight:700;color:#eceff1;">Gang Chat</div><div style="font-size:12px;line-height:18px;color:#6f7785;">账号安全验证</div></td>
          </tr></table>
        </td></tr>
        <tr><td style="padding:30px;">
          <div style="font-size:20px;line-height:28px;font-weight:700;color:#eceff1;">%s</div>
          <div style="margin-top:10px;font-size:14px;line-height:22px;color:#b0b8c0;">%s</div>
          <div style="margin-top:22px;padding:18px 16px;text-align:center;background:#13241e;border:1px solid #34765d;border-radius:12px;">
            <div style="font-size:12px;line-height:18px;color:#6fcfa6;letter-spacing:1px;">验证码</div>
            <div style="margin-top:6px;font-size:32px;line-height:40px;font-weight:700;letter-spacing:8px;color:#6fcfa6;">%s</div>
          </div>
          <div style="margin-top:20px;padding:12px 14px;background:#14171d;border-radius:10px;font-size:13px;line-height:21px;color:#b0b8c0;">
            验证码将在 <strong style="color:#eceff1;">10 分钟</strong>后失效。请勿将验证码告诉任何人，Gang Chat 工作人员不会向您索取验证码。
          </div>
          <div style="margin-top:20px;font-size:12px;line-height:19px;color:#6f7785;">%s</div>
        </td></tr>
        <tr><td style="padding:16px 30px;background:#14171d;border-top:1px solid #2a2f38;font-size:11px;line-height:18px;color:#6f7785;text-align:center;">此邮件由 Gang Chat 账号安全服务自动发送</td></tr>
      </table>
    </td></tr>
  </table>
</body>
</html>`, escapedPreview, escapedTitle, escapedDescription, escapedCode, escapedFooter)
}
