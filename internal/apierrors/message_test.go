package apierrors

import "testing"

func TestUserMessage(t *testing.T) {
	tests := []struct {
		name    string
		code    string
		message string
		status  int
		want    string
	}{
		{name: "keeps Chinese", code: "bad_request", message: "请输入验证码", status: 400, want: "请输入验证码"},
		{name: "known detail", code: "not_found", message: "sticker not found", status: 404, want: "表情不存在"},
		{name: "suspended account", code: "account_suspended", message: "account suspended", status: 403, want: "账号已被封禁"},
		{name: "code fallback", code: "validation_failed", message: "field xyz is malformed", status: 400, want: "请求内容不符合要求"},
		{name: "status fallback", code: "unknown", message: "upstream exploded", status: 503, want: "服务器暂时无法完成请求，请稍后重试"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UserMessage(tt.code, tt.message, tt.status); got != tt.want {
				t.Fatalf("UserMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}
