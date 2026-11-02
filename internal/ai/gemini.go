package ai

import (
	"chatbot-go/internal/redact"
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
)

const geminiBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// Gemini Google Gemini（REST，金鑰放在 header，不放 URL）.
type Gemini struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

// NewGemini 建立 Gemini 供應商；apiKey 為空時回傳 nil（不啟用）.
func NewGemini(apiKey string, client *http.Client) Provider {
	if apiKey == "" {
		return nil
	}
	return &Gemini{apiKey: apiKey, baseURL: geminiBaseURL, client: client}
}

// Name 供應商代號.
func (g *Gemini) Name() string { return ProviderGemini }

func (g *Gemini) headers() map[string]string {
	return map[string]string{"x-goog-api-key": g.apiKey}
}

// Candidates Flash 系列都能看圖，文字與看圖共用同一份排序.
func (g *Gemini) Candidates(ctx context.Context, _ Capability) ([]string, error) {
	var resp struct {
		Models []struct {
			Name    string   `json:"name"`
			Methods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := doJSON(ctx, g.client, http.MethodGet, g.baseURL+"/models?pageSize=200", g.headers(), nil, &resp); err != nil {
		// 列不出來仍可用別名.
		slog.Warn("Gemini model listing failed, using the alias", "error", redact.Error(err))
		return []string{geminiFallbackAlias}, nil
	}
	var names []string
	for _, m := range resp.Models {
		for _, method := range m.Methods {
			if method == "generateContent" {
				names = append(names, strings.TrimPrefix(m.Name, "models/"))
				break
			}
		}
	}
	return rankGemini(names), nil
}

type geminiPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inline_data,omitempty"`
}

type geminiInlineData struct {
	MIMEType string `json:"mime_type"`
	Data     string `json:"data"`
}

// Call 呼叫 generateContent.
func (g *Gemini) Call(ctx context.Context, model string, req *Request) (string, error) {
	parts := []geminiPart{{Text: req.Prompt}}
	for _, img := range req.Images {
		parts = append(parts, geminiPart{InlineData: &geminiInlineData{MIMEType: img.MIME, Data: base64.StdEncoding.EncodeToString(img.Data)}})
	}
	body := map[string]any{"contents": []map[string]any{{"parts": parts}}}
	if req.System != "" {
		body["systemInstruction"] = map[string]any{"parts": []geminiPart{{Text: req.System}}}
	}
	if req.JSON {
		body["generationConfig"] = map[string]any{"responseMimeType": "application/json"}
	}
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []geminiPart `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	url := g.baseURL + "/models/" + model + ":generateContent"
	if err := doJSON(ctx, g.client, http.MethodPost, url, g.headers(), body, &resp); err != nil {
		return "", err
	}
	if len(resp.Candidates) == 0 {
		return "", ErrEmpty
	}
	var sb strings.Builder
	for _, p := range resp.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	return sb.String(), nil
}
