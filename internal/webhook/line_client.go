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

const replyTokenLogPrefixLen = 8

var (
	httpClient = &http.Client{Timeout: 30 * time.Second}
	getConfig  = config.Get
)

var processSem = make(chan struct{}, 50)

// ReplyText 回覆文字訊息給 LINE 使用者.
func ReplyText(ctx context.Context, replyToken, text string) error {
	cfg := getConfig()

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
		slog.Warn("LINE reply API non-200", "status", resp.StatusCode, "replyToken", maskedReplyToken(replyToken))
	}

	return nil
}

// ReplyFlex 回覆 Flex Message 給 LINE 使用者.
func ReplyFlex(ctx context.Context, replyToken, altText string, contents any) error {
	cfg := config.Get()

	reply := models.FlexReplyMessage{
		ReplyToken: replyToken,
		Messages: []models.FlexMessage{
			{
				Type:     "flex",
				AltText:  altText,
				Contents: contents,
			},
		},
	}

	body, err := json.Marshal(reply)
	if err != nil {
		return fmt.Errorf("marshal flex reply: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Line.ReplyURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Line.ChannelToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send flex reply: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("LINE flex reply API non-200", "status", resp.StatusCode, "replyToken", maskedReplyToken(replyToken))
	}

	return nil
}

func maskedReplyToken(replyToken string) string {
	if len(replyToken) <= replyTokenLogPrefixLen {
		return replyToken
	}
	return replyToken[:replyTokenLogPrefixLen] + "..."
}
