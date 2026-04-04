package webhook

import (
	"bytes"
	"chatbot-go/internal/models"
	"chatbot-go/internal/platform/config"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// ReplyText 回覆文字訊息給 LINE 使用者.
func ReplyText(ctx context.Context, replyToken, text string) error {
	cfg := config.Get()

	reply := models.ReplyMessage{
		ReplyToken: replyToken,
		Messages: []models.ReplyBody{
			{Type: "text", Text: text},
		},
	}

	body, err := json.Marshal(reply)
	if err != nil {
		return fmt.Errorf("marshal reply: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Line.ReplyURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Line.ChannelToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send reply: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("LINE reply API non-200", "status", resp.StatusCode, "replyToken", replyToken[:8]+"...")
	}

	return nil
}
