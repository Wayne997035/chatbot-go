package ai

import (
	"context"
	"encoding/base64"
	"net/http"
	"slices"
	"strings"
)

// OpenAICompatible 走 OpenAI chat completions 格式的供應商（Groq、NVIDIA）.
type OpenAICompatible struct {
	name    string
	apiKey  string
	baseURL string
	client  *http.Client
	text    rankRules
	vision  rankRules
	// jsonMode 是否送 response_format=json_object（Groq 支援；NVIDIA 各模型不一，不送）.
	jsonMode bool
	// maxTokens 0 表示不指定（Groq 會依請求估算額度，指定過大反而撞每分鐘上限）.
	maxTokens int
}

const (
	groqBaseURL   = "https://api.groq.com/openai/v1"
	nvidiaBaseURL = "https://integrate.api.nvidia.com/v1"
)

// 看圖模型通常在名稱上有這些字.
var visionMarkers = []string{"vision", "-vl", "vl-", "llama-4", "scout", "maverick", "gemma-3", "multimodal", "omni"}

// [F23-01] Groq 的 /models 沒有能力欄位，只能靠名稱辨識看圖模型；
// 這份清單來自 Groq 看圖文件 https://console.groq.com/docs/vision .
var groqVisionModels = []string{"qwen3.8-27b", "muse-spark"}

// NewGroq 建立 Groq 供應商；apiKey 為空時回傳 nil.
func NewGroq(apiKey string, client *http.Client) Provider {
	if apiKey == "" {
		return nil
	}
	skip := []string{"whisper", "guard", "tts", "playai", "embed", "orpheus", "compound"}
	return &OpenAICompatible{
		name: ProviderGroq, apiKey: apiKey, baseURL: groqBaseURL, client: client, jsonMode: true,
		text: rankRules{families: []string{"gpt-oss", "llama", "qwen", "kimi", "deepseek", "mistral", "gemma"}, skip: skip},
		// [F23-01] 官方現行看圖模型排最前；slices.Concat 回傳新切片，不改到與 NVIDIA 共用的 visionMarkers.
		vision: rankRules{
			families: []string{"qwen3.8-27b", "muse-spark", "maverick", "scout", "llama-4", "gemma"},
			skip:     skip,
			require:  slices.Concat(visionMarkers, groqVisionModels),
		},
	}
}

// NewNVIDIA 建立 NVIDIA API Catalog 供應商；apiKey 為空時回傳 nil.
// 注意：NVIDIA 免費 API Catalog 依試用條款僅限開發與評估用途.
func NewNVIDIA(apiKey string, client *http.Client) Provider {
	if apiKey == "" {
		return nil
	}
	common := []string{
		"embed", "rerank", "retriev", "guard", "safety", "reward", "clip", "parse", "ocr", "deplot", "riva",
		"asr", "tts", "translate", "coder", "math", "pii", "detector", "classifier", "code", "chatqa", "base", "cosmos",
	}
	textSkip := append([]string{"vision", "-vl", "vlm", "kosmos", "fuyu", "paligemma", "neva"}, common...)
	return &OpenAICompatible{
		name: ProviderNVIDIA, apiKey: apiKey, baseURL: nvidiaBaseURL, client: client, maxTokens: 4096,
		text:   rankRules{families: []string{"gpt-oss", "llama", "nemotron", "qwen", "deepseek", "kimi", "mistral"}, skip: textSkip},
		vision: rankRules{families: []string{"llama-4", "gemma-3", "nemotron", "llama-3.2", "phi"}, skip: common, require: visionMarkers},
	}
}

// Name 供應商代號.
func (o *OpenAICompatible) Name() string { return o.name }

func (o *OpenAICompatible) headers() map[string]string {
	return map[string]string{"Authorization": "Bearer " + o.apiKey}
}

// Candidates 列出模型並依能力排序.
func (o *OpenAICompatible) Candidates(ctx context.Context, c Capability) ([]string, error) {
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := doJSON(ctx, o.client, http.MethodGet, o.baseURL+"/models", o.headers(), nil, &resp); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		ids = append(ids, m.ID)
	}
	if c == Vision {
		return rankModels(ids, &o.vision), nil
	}
	return rankModels(ids, &o.text), nil
}

// Call 呼叫 chat completions.
func (o *OpenAICompatible) Call(ctx context.Context, model string, req *Request) (string, error) {
	var content any = req.Prompt
	if len(req.Images) > 0 {
		parts := []map[string]any{{"type": "text", "text": req.Prompt}}
		for _, img := range req.Images {
			url := "data:" + img.MIME + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}})
		}
		content = parts
	}
	messages := []map[string]any{{"role": "user", "content": content}}
	if req.System != "" {
		messages = append([]map[string]any{{"role": "system", "content": req.System}}, messages...)
	}
	body := map[string]any{
		"model":       model,
		"messages":    messages,
		"temperature": 0.3,
	}
	if o.jsonMode && req.JSON {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	if o.maxTokens > 0 {
		body["max_tokens"] = o.maxTokens
	}
	if o.name == ProviderGroq && strings.Contains(model, "gpt-oss") {
		// 預設推理強度會把 token 花在推理上，JSON 還沒寫完就到上限（Groq gpt-oss-20b 的教訓）.
		body["reasoning_effort"] = "low"
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := doJSON(ctx, o.client, http.MethodPost, o.baseURL+"/chat/completions", o.headers(), body, &resp); err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", ErrEmpty
	}
	return resp.Choices[0].Message.Content, nil
}
