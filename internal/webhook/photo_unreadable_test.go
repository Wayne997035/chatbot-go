package webhook

import (
	"bytes"
	"chatbot-go/internal/ai"
	"chatbot-go/internal/nutrition"
	"context"
	"image"
	"image/jpeg"
	"testing"
)

// photoUnreadableProvider [F37-34] 計數用的假供應商；名稱 groq 在 Router 的 VisionOrder 內，所以對這個 Router 可達.
type photoUnreadableProvider struct {
	calls int
	err   error
}

func (*photoUnreadableProvider) Name() string { return "groq" }

func (*photoUnreadableProvider) Candidates(context.Context, ai.Capability) ([]string, error) {
	return []string{"vision-model"}, nil
}

func (p *photoUnreadableProvider) Call(context.Context, string, *ai.Request) (string, error) {
	p.calls++
	return "", p.err
}

// photoUnreadableJPEG 一張合法的小 JPEG（有 EOI）.
func photoUnreadableJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// photoUnreadableReplyText 最後一則回覆的第一個 text 訊息（整句比對用）.
func photoUnreadableReplyText(t *testing.T, api *lineAPI) string {
	t.Helper()
	if len(api.replies) == 0 {
		t.Fatal("no reply sent")
	}
	msgs, _ := api.replies[len(api.replies)-1]["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("reply has no messages: %v", api.replies[len(api.replies)-1])
	}
	first, _ := msgs[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

// [F37-34] 走完整個 webhook 照片路徑（真 ai.Router + nutrition.NewAnalyzer）：
// 壞檔回新固定句且供應商 0 次；合法 JPEG 但供應商全失敗回既有句且供應商恰 1 次（正對照）.
func TestPhotoUnreadableReplyVersusUnavailable(t *testing.T) {
	good := photoUnreadableJPEG(t)
	tests := []struct {
		name      string
		content   []byte
		wantReply string
		wantCalls int
	}{
		{"truncated_jpeg_no_eoi", good[:len(good)-2], "這張照片無法讀取，請換一張再試", 0},
		{"truncated_in_scan", good[:len(good)/2], "這張照片無法讀取，請換一張再試", 0},
		{"valid_jpeg_all_providers_fail", good, "目前無法分析照片，請稍後再試", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newLineAPI(t, tt.content)
			prov := &photoUnreadableProvider{err: &ai.APIError{Status: 500, Message: "boom"}}
			router := ai.NewRouter(&ai.Settings{VisionOrder: []string{"groq"}, TextOrder: []string{"groq"}}, prov)
			h := NewWebhookHandler(api.client, nil, nil, nil, nil).WithNutrition(nutrition.NewAnalyzer(router), nil, nil)

			h.handleMessage(context.Background(), imageEvent())

			if got := photoUnreadableReplyText(t, api); got != tt.wantReply {
				t.Errorf("reply = %q, want %q", got, tt.wantReply)
			}
			if prov.calls != tt.wantCalls {
				t.Errorf("provider calls = %d, want %d", prov.calls, tt.wantCalls)
			}
		})
	}
}
