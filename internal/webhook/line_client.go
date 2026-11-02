package webhook

import (
	"bytes"
	"chatbot-go/internal/models"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/redact"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

var processSem = make(chan struct{}, 50)

// Reply 回覆一到五則訊息給 LINE 使用者.
func Reply(ctx context.Context, replyToken string, messages ...any) error {
	return postLINE(ctx, config.Get().Line.ReplyURL, map[string]any{"replyToken": replyToken, "messages": messages})
}

// ReplyText 回覆文字訊息；可附 Quick Reply 按鈕.
func ReplyText(ctx context.Context, replyToken, text string, items ...models.QuickReplyItem) error {
	return Reply(ctx, replyToken, models.NewText(text, items...))
}

// ReplyFlex 回覆 Flex Message 給 LINE 使用者.
func ReplyFlex(ctx context.Context, replyToken, altText string, contents any) error {
	return Reply(ctx, replyToken, models.FlexMessage{Type: "flex", AltText: altText, Contents: contents})
}

// postLINE 呼叫 LINE Messaging API；非 200 回傳錯誤（訊息已遮蔽），不再默默吞掉.
func postLINE(ctx context.Context, url string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal LINE payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create LINE request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+config.Get().Line.ChannelToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send LINE request: %s", redact.Error(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("LINE API returned %d: %s", resp.StatusCode, redact.String(string(detail)))
	}
	return nil
}
