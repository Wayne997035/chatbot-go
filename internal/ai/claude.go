package ai

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// DefaultClaudeModel 自由問答使用的 Claude 模型.
const DefaultClaudeModel = "claude-opus-5-5"

// claudeMaxTokens 問答回覆的上限；聊天回覆很短，給足空間避免截斷.
const claudeMaxTokens = 4096

// Claude Anthropic Claude（官方 Go SDK）.
//
// 被安全分類器拒答時，伺服器端 fallback（server-side-fallback）會改由其他 Claude 模型回答；
// 仍然拒答則回傳錯誤，讓路由換下一家.
type Claude struct {
	client anthropic.Client
	model  string
}

// NewClaude 建立 Claude 供應商；apiKey 為空時回傳 nil. model 為空時用 DefaultClaudeModel.
func NewClaude(apiKey, model string, opts ...option.RequestOption) Provider {
	if apiKey == "" {
		return nil
	}
	if model == "" {
		model = DefaultClaudeModel
	}
	// 重試交給路由（換模型、換供應商、短暫限流才等），SDK 不自己重試.
	opts = append([]option.RequestOption{option.WithAPIKey(apiKey), option.WithMaxRetries(0)}, opts...)
	return &Claude{client: anthropic.NewClient(opts...), model: model}
}

// Name 供應商代號.
func (c *Claude) Name() string { return ProviderClaude }

// Candidates 只用設定的模型（Claude 的模型清單不需要自動排序）.
func (c *Claude) Candidates(context.Context, Capability) ([]string, error) {
	return []string{c.model}, nil
}

// errRefused 安全分類器拒答.
var errRefused = errors.New("claude declined the request")

// Call 呼叫 Messages API.
func (c *Claude) Call(ctx context.Context, model string, req *Request) (string, error) {
	blocks := []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(req.Prompt)}
	for _, img := range req.Images {
		blocks = append(blocks, anthropic.NewBetaImageBlock(anthropic.BetaBase64ImageSourceParam{
			Data:      base64.StdEncoding.EncodeToString(img.Data),
			MediaType: anthropic.BetaBase64ImageSourceMediaType(img.MIME),
		}))
	}
	params := anthropic.BetaMessageNewParams{
		Model:     model,
		MaxTokens: claudeMaxTokens,
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(blocks...)},
		// 聊天問答不需要深度推理；低 effort 回覆更快、更省.
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortLow},
		Fallbacks:    anthropic.BetaFallbacksParamOfDefault(),
		Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	}
	if req.System != "" {
		// 固定的系統指示放最前面並標記快取，重複請求只算快取讀取價.
		params.System = []anthropic.BetaTextBlockParam{{Text: req.System, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}}
	}

	msg, err := c.client.Beta.Messages.New(ctx, params)
	if err != nil {
		return "", toAPIError(err)
	}
	if msg.StopReason == anthropic.BetaStopReasonRefusal {
		return "", errRefused
	}
	var sb strings.Builder
	for i := range msg.Content {
		if text, ok := msg.Content[i].AsAny().(anthropic.BetaTextBlock); ok {
			sb.WriteString(text.Text)
		}
	}
	return sb.String(), nil
}

// toAPIError 把 SDK 錯誤轉成路由認得的 APIError（429 限流、404 模型不存在等才能正確分類）.
func toAPIError(err error) error {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	out := &APIError{Status: apiErr.StatusCode, Message: errorMessage([]byte(apiErr.RawJSON()))}
	if apiErr.Response != nil {
		out.RetryAfter = retryAfterHeader(apiErr.Response.Header.Get("Retry-After"))
	}
	return out
}
