package chat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zhuangkaiyi/gang-chat/server/internal/config"
)

type recordedReply struct {
	roomID  string
	trigger gangBotTrigger
}

func recordingBot() (*gangBot, *[]recordedReply, *sync.Mutex) {
	bot := newGangBot(nil)
	var mu sync.Mutex
	var calls []recordedReply
	bot.respond = func(_ context.Context, roomID string, trigger gangBotTrigger) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, recordedReply{roomID, trigger})
	}
	return bot, &calls, &mu
}

func TestGangBotSpeaksUpEveryThreeToFiveMessages(t *testing.T) {
	bot, calls, mu := recordingBot()
	var at []int
	for i := 1; i <= 200; i++ {
		before := len(*calls)
		bot.observe("room", "user", "m", false)
		bot.wait()
		if len(*calls) > before {
			at = append(at, i)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(at) < 200/gangBotMaxGap || len(at) > 200/gangBotMinGap {
		t.Fatalf("spoke %d times in 200 messages", len(at))
	}
	prev := 0
	seen := map[int]bool{}
	for _, n := range at {
		gap := n - prev
		if gap < gangBotMinGap || gap > gangBotMaxGap {
			t.Fatalf("gap of %d messages between replies (at %v)", gap, at)
		}
		seen[gap] = true
		prev = n
	}
	if len(seen) < 2 {
		t.Fatalf("gaps never varied: %v", at)
	}
}

func TestGangBotAnswersMentionsAtOnceAndIgnoresItself(t *testing.T) {
	bot, calls, _ := recordingBot()
	bot.observe("room", gangBotSenderID, "own", true) // its own message never counts
	bot.observe("room", "user", "m1", true)
	bot.wait()
	if len(*calls) != 1 || (*calls)[0].trigger.mentionID != "m1" {
		t.Fatalf("calls = %+v", *calls)
	}
	// Rooms are counted separately.
	bot.observe("other", "user", "x", false)
	bot.wait()
	if len(*calls) != 1 {
		t.Fatalf("a single message in another room triggered a reply")
	}
}

func TestGangBotQueuesAMentionThatArrivesMidReply(t *testing.T) {
	bot := newGangBot(nil)
	release := make(chan struct{})
	var mu sync.Mutex
	var got []string
	bot.respond = func(_ context.Context, _ string, trigger gangBotTrigger) {
		mu.Lock()
		got = append(got, trigger.mentionID)
		first := len(got) == 1
		mu.Unlock()
		if first {
			<-release
		}
	}
	bot.observe("room", "user", "first", true)
	bot.observe("room", "user", "second", true) // the bot is still busy
	bot.observe("room", "user", "third", true)  // replaces the queued one
	close(release)
	bot.wait()
	if strings.Join(got, ",") != "first,third" {
		t.Fatalf("replies = %v", got)
	}
}

func TestGangBotRecognizesMentions(t *testing.T) {
	bot := newGangBot(nil)
	cases := []struct {
		mentions []any
		body     string
		want     bool
	}{
		{[]any{map[string]any{"type": "bot"}}, "@gang-bot 在吗", true},
		{[]any{map[string]any{"type": "bot"}}, "在吗", true},
		{nil, "@Gang-Bot 你怎么看", true},
		{[]any{map[string]any{"type": "user", "user_id": "someone"}}, "@someone hi", false},
		{[]any{map[string]any{"type": "all"}}, "@所有人 开会", false},
	}
	for _, c := range cases {
		if got := bot.mentions(c.mentions, c.body); got != c.want {
			t.Errorf("mentions(%v, %q) = %v", c.mentions, c.body, got)
		}
	}
}

func TestGangBotPromptShowsTheTruncatedWindow(t *testing.T) {
	wang := userSummary{ID: "u2", Username: "wang", DisplayName: "小王"}
	bot := gangBotSummary()
	recent := []message{
		{ID: "m1", Type: "text", Body: "今晚开黑吗\n八点", Sender: wang, CreatedAt: "2026-10-15T12:03:00.000Z"},
		{ID: "m2", Type: "text", Body: "算我一个", Sender: bot, CreatedAt: "2026-10-15T12:04:00.000Z"},
		{ID: "m3", Type: "text", Body: "撤回了", IsRecalled: true, Sender: wang, CreatedAt: "2026-10-15T12:05:00.000Z"},
		{ID: "m4", Type: "text", Body: "@gang-bot 你玩什么位置", Sender: wang, CreatedAt: "2026-10-15T12:06:00.000Z",
			Quotes: []messageQuote{{SenderDisplayName: "gang-bot", Body: "算我一个"}}},
	}
	prompt := gangBotPrompt("主群", recent, &recent[3])
	for _, want := range []string{
		"「主群」里最近的 4 条消息",
		"只有这 10 条，更早的你看不到",
		"[10-15 20:03] 小王：今晚开黑吗\n    八点",
		"gang-bot（你）：算我一个",
		"[消息已撤回]",
		"（回复 gang-bot：「算我一个」）",
		"小王 @ 了你",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "撤回了") {
		t.Error("a recalled message's text leaked into the prompt")
	}
	if !strings.Contains(gangBotPrompt("", recent[:1], nil), "没有人 @ 你") {
		t.Error("unprompted replies should be told nobody asked")
	}
	if !strings.Contains(gangBotSystemPrompt, "最近的 10 条消息") || !strings.Contains(gangBotSystemPrompt, "截掉") {
		t.Error("the system prompt must say only the last ten messages are visible")
	}
}

func TestGangBotIsNotAUser(t *testing.T) {
	summary := gangBotSummary()
	if !summary.IsBot || summary.ID != gangBotSenderID || summary.DisplayName != "gang-bot" {
		t.Fatalf("summary = %+v", summary)
	}
	// Room payloads carry the switch as two flat fields.
	raw, _ := json.Marshal(roomCard{ID: "r1", gangBotRoomFields: gangBotRoomFields{GangBotAvailable: true, GangBotEnabled: false}})
	var card map[string]any
	_ = json.Unmarshal(raw, &card)
	if card["gang_bot_available"] != true || card["gang_bot_enabled"] != false {
		t.Fatalf("room card JSON = %s", raw)
	}
	raw, _ = json.Marshal(summary)
	if !strings.Contains(string(raw), `"is_bot":true`) {
		t.Fatalf("sender JSON = %s", raw)
	}
}

func TestCleanGangBotReply(t *testing.T) {
	cases := map[string]string{
		"  好啊，八点见  ":                  "好啊，八点见",
		"gang-bot：好啊":                 "好啊",
		"[10-15 20:04] gang-bot（你）：好": "好",
		"「打野吧」":                       "打野吧",
		"一\n\n\n\n二":                  "一\n\n二",
		"":                            "",
	}
	for in, want := range cases {
		if got := cleanGangBotReply(in); got != want {
			t.Errorf("cleanGangBotReply(%q) = %q, want %q", in, got, want)
		}
	}
	if got := cleanGangBotReply(strings.Repeat("长", 2000)); len([]rune(got)) != gangBotMaxReplyRunes+1 {
		t.Errorf("long replies should be cut to %d runes plus an ellipsis, got %d", gangBotMaxReplyRunes, len([]rune(got)))
	}
}

func TestAPIRootURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.anthropic.com":                    "https://api.anthropic.com/",
		"https://proxy.example.com/v1/":                "https://proxy.example.com/",
		"https://proxy.example.com/claude/v1/messages": "https://proxy.example.com/claude/",
	} {
		if got := apiRootURL(in); got != want {
			t.Errorf("apiRootURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGangBotSettingsAreAllRequired(t *testing.T) {
	cfg := config.Config{GangBotAPIURL: "https://api.anthropic.com", GangBotAPIKey: "key"}
	if cfg.GangBotEnabled() || strings.Join(cfg.GangBotMissingSettings(), ",") != "gang_bot_model" {
		t.Fatalf("missing = %v", cfg.GangBotMissingSettings())
	}
	cfg.GangBotModel = "claude-opus-5-5"
	if !cfg.GangBotEnabled() {
		t.Fatal("all three set should enable the bot")
	}
}

// messagesServer stands in for the Messages API and records requests.
func messagesServer(t *testing.T, handle func(body map[string]any, r *http.Request) (int, string)) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		body["_path"] = r.URL.Path
		body["_key"] = r.Header.Get("X-Api-Key")
		body["_version"] = r.Header.Get("Anthropic-Version")
		body["_beta"] = r.Header.Get("Anthropic-Beta")
		requests = append(requests, body)
		status, response := handle(body, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

const okMessage = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
  "content":[{"type":"text","text":"好啊，算我一个"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`

func TestAnthropicModelSendsAMessagesRequest(t *testing.T) {
	server, requests := messagesServer(t, func(map[string]any, *http.Request) (int, string) { return 200, okMessage })
	model := newAnthropicGangBotModel(server.URL+"/v1", "test-key", "claude-opus-5-5")
	reply, err := model.Reply(context.Background(), "system text", "prompt text")
	if err != nil || reply != "好啊，算我一个" {
		t.Fatalf("reply = %q, err = %v", reply, err)
	}
	req := (*requests)[0]
	if req["_path"] != "/v1/messages" || req["_key"] != "test-key" || req["_version"] == "" {
		t.Fatalf("request = %v", req)
	}
	if req["model"] != "claude-opus-5-5" || req["fallbacks"] != "default" || !strings.Contains(req["_beta"].(string), "server-side-fallback-2026-07-01") {
		t.Fatalf("request = %v", req)
	}
	system, _ := json.Marshal(req["system"])
	messages, _ := json.Marshal(req["messages"])
	output, _ := json.Marshal(req["output_config"])
	if !strings.Contains(string(system), "system text") || !strings.Contains(string(messages), "prompt text") || !strings.Contains(string(output), `"low"`) {
		t.Fatalf("system=%s messages=%s output_config=%s", system, messages, output)
	}
}

func TestAnthropicModelDropsFallbacksWhenAProxyRejectsThem(t *testing.T) {
	server, requests := messagesServer(t, func(body map[string]any, _ *http.Request) (int, string) {
		if _, ok := body["fallbacks"]; ok {
			return 400, `{"type":"error","error":{"type":"invalid_request_error","message":"fallbacks: Extra inputs are not permitted"}}`
		}
		return 200, okMessage
	})
	model := newAnthropicGangBotModel(server.URL, "k", "some-model")
	for i := 0; i < 2; i++ {
		if reply, err := model.Reply(context.Background(), "s", "p"); err != nil || reply == "" {
			t.Fatalf("call %d: reply = %q, err = %v", i, reply, err)
		}
	}
	if len(*requests) != 3 {
		t.Fatalf("expected one rejected try and two plain calls, got %d requests", len(*requests))
	}
}

func TestAnthropicModelReportsRefusals(t *testing.T) {
	server, _ := messagesServer(t, func(map[string]any, *http.Request) (int, string) {
		return 200, `{"id":"m","type":"message","role":"assistant","model":"x","content":[],"stop_reason":"refusal","usage":{"input_tokens":1,"output_tokens":0}}`
	})
	_, err := newAnthropicGangBotModel(server.URL, "k", "x").Reply(context.Background(), "s", "p")
	if !errors.Is(err, errGangBotRefused) {
		t.Fatalf("err = %v", err)
	}
}
