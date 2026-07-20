package apierrors

import (
	"fmt"
	"strings"
	"unicode"
)

// UserMessage guarantees that API error copy exposed to clients is
// Simplified Chinese. Internal logs keep the original error details.
func UserMessage(code, message string, status int) string {
	trimmed := strings.TrimSpace(message)
	if containsHan(trimmed) {
		return trimmed
	}

	normalized := strings.ToLower(trimmed)
	if translated := knownMessage(normalized); translated != "" {
		return translated
	}

	switch strings.ToLower(strings.TrimSpace(code)) {
	case "unauthorized", "invalid_credentials":
		return "登录状态无效，请重新登录"
	case "forbidden":
		return "没有权限执行此操作"
	case "not_found":
		return "请求的内容不存在"
	case "conflict", "idempotency_conflict":
		return "当前操作与已有状态冲突"
	case "blocked":
		return "当前用户已被该房间屏蔽"
	case "rate_limited":
		return "操作过于频繁，请稍后重试"
	case "bad_request", "validation_failed":
		return "请求内容不符合要求"
	case "confirmation_required":
		return "需要确认后才能继续"
	case "payload_too_large":
		return "上传的文件过大"
	case "email_unavailable":
		return "邮件发送服务暂时不可用"
	case "email_send_failed":
		return "验证码邮件发送失败，请稍后重试"
	case "email_verification_required":
		return "请先验证邮箱"
	case "password_reset_verification_required":
		return "请先验证绑定邮箱"
	case "account_not_found":
		return "该用户名或邮箱对应的账号不存在"
	case "account_suspended":
		return "账号已被封禁"
	case "verification_expired", "challenge_not_found":
		return "验证码已失效，请重新获取"
	case "invalid_verification_code":
		return "验证码错误"
	case "livekit_error":
		return "语音服务暂时无法完成操作"
	case "livekit_unavailable":
		return "语音服务暂时不可用"
	case "screen_share_not_active":
		return "屏幕共享已结束"
	case "music_box_unavailable":
		return "音乐盒服务暂时不可用"
	case "upstream_error":
		return "上游服务暂时不可用"
	case "storage_unavailable":
		return "文件存储服务暂时不可用"
	case "stream_unavailable":
		return "实时连接服务暂时不可用"
	case "sticker_asset_expired":
		return "原表情文件已失效"
	case "internal_error":
		return "服务器暂时无法完成请求，请稍后重试"
	default:
		return statusMessage(status)
	}
}

func knownMessage(message string) string {
	switch {
	case strings.Contains(message, "invalid credentials"):
		return "账号或密码不正确"
	case strings.Contains(message, "too many failed login attempts"):
		return "登录尝试次数过多，请稍后再试"
	case strings.Contains(message, "session revoked"):
		return "登录会话已被撤销"
	case strings.Contains(message, "session expired"):
		return "登录会话已过期"
	case strings.Contains(message, "session invalid"):
		return "登录会话无效，请重新登录"
	case strings.Contains(message, "invalid refresh token"),
		strings.Contains(message, "invalid token"),
		strings.Contains(message, "missing authorization"):
		return "登录状态无效，请重新登录"
	case strings.Contains(message, "user inactive"):
		return "账号当前不可用"
	case strings.Contains(message, "account suspended"):
		return "账号已被封禁"
	case strings.Contains(message, "current password incorrect"):
		return "当前密码不正确"
	case strings.Contains(message, "username or email already taken"):
		return "登录用户名或邮箱已被占用"
	case strings.Contains(message, "username, email or phone number already taken"):
		return "登录用户名、邮箱或手机号已被占用"
	case strings.Contains(message, "room not found"):
		return "房间不存在"
	case strings.Contains(message, "quoted message is unavailable"):
		return "被引用的消息不可用"
	case strings.Contains(message, "message not found"), strings.Contains(message, "message does not exist"):
		return "消息不存在"
	case strings.Contains(message, "sticker file not found"):
		return "表情文件不存在"
	case strings.Contains(message, "sticker pack not found"):
		return "表情包不存在"
	case strings.Contains(message, "sticker not found"):
		return "表情不存在"
	case strings.Contains(message, "member not found"):
		return "房间成员不存在"
	case strings.Contains(message, "user not found"):
		return "用户不存在"
	case strings.Contains(message, "admin required"):
		return "需要管理员权限"
	case strings.Contains(message, "owner required"):
		return "需要房主权限"
	case strings.Contains(message, "uploaded file is too large"):
		return "上传的文件过大"
	case strings.Contains(message, "invalid json body"), strings.Contains(message, "invalid request body"):
		return "请求内容格式错误"
	}
	return ""
}

func statusMessage(status int) string {
	switch status {
	case 400:
		return "请求内容不符合要求"
	case 401:
		return "登录状态无效，请重新登录"
	case 403:
		return "没有权限执行此操作"
	case 404:
		return "请求的内容不存在"
	case 409:
		return "当前操作与已有状态冲突"
	case 413:
		return "上传的文件过大"
	case 429:
		return "操作过于频繁，请稍后重试"
	default:
		if status >= 500 {
			return "服务器暂时无法完成请求，请稍后重试"
		}
		return fmt.Sprintf("请求失败（状态码 %d）", status)
	}
}

func containsHan(value string) bool {
	for _, r := range value {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
