package chat

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// gangBotModel turns the bot's prompt into one chat message. It is an
// interface so tests can swap in a canned model.
type gangBotModel interface {
	Reply(ctx context.Context, system, prompt string) (string, error)
}

// anthropicGangBotModel calls the Anthropic Messages API (or a compatible
// endpoint) through the official SDK.
type anthropicGangBotModel struct {
	client anthropic.Client
	model  string
	// Server-side refusal fallbacks are on until an endpoint (a proxy, say)
	// rejects them; then the bot keeps working without.
	fallbacks atomic.Bool
}

func newAnthropicGangBotModel(baseURL, apiKey, model string) *anthropicGangBotModel {
	m := &anthropicGangBotModel{
		client: anthropic.NewClient(
			option.WithBaseURL(apiRootURL(baseURL)),
			option.WithAPIKey(apiKey),
			option.WithMaxRetries(2),
			option.WithRequestTimeout(90*time.Second),
		),
		model: model,
	}
	m.fallbacks.Store(true)
	return m
}

// apiRootURL accepts the API root with or without a trailing /v1 or
// /v1/messages; the SDK appends the path itself.
func apiRootURL(raw string) string {
	url := strings.TrimRight(strings.TrimSpace(raw), "/")
	url = strings.TrimSuffix(url, "/v1/messages")
	url = strings.TrimSuffix(url, "/v1")
	return url + "/"
}

func (m *anthropicGangBotModel) Reply(ctx context.Context, system, prompt string) (string, error) {
	resp, err := m.send(ctx, system, prompt, m.fallbacks.Load())
	if err != nil && m.fallbacks.Load() && rejectsFallbacks(err) {
		m.fallbacks.Store(false)
		resp, err = m.send(ctx, system, prompt, false)
	}
	if err != nil {
		return "", err
	}
	if resp.StopReason == anthropic.BetaStopReasonRefusal {
		return "", errGangBotRefused
	}
	var text strings.Builder
	for _, block := range resp.Content {
		if b, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			text.WriteString(b.Text)
		}
	}
	// It ran out of tokens mid-reply: say so rather than end abruptly.
	if resp.StopReason == anthropic.BetaStopReasonMaxTokens && text.Len() > 0 {
		text.WriteString("\n（太长了，没写完）")
	}
	return text.String(), nil
}

var errGangBotRefused = errors.New("gang-bot: the model declined to answer")

func (m *anthropicGangBotModel) send(ctx context.Context, system, prompt string, fallbacks bool) (*anthropic.BetaMessage, error) {
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(m.model),
		MaxTokens: 8000,
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(prompt)),
		},
		// Group chat wants quick, short answers more than deep reasoning.
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortLow},
	}
	if fallbacks {
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
	}
	return m.client.Beta.Messages.New(ctx, params)
}

// rejectsFallbacks recognizes an endpoint refusing the fallbacks field or its
// beta header, as Anthropic-compatible proxies may.
func rejectsFallbacks(err error) bool {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		return false
	}
	message := strings.ToLower(apiErr.Error())
	return strings.Contains(message, "fallback") || strings.Contains(message, "beta")
}
