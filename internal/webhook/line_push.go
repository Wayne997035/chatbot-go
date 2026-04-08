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
)

// LinePushNotifier LINE Push 推播實作.
type LinePushNotifier struct{}

// Push 推播文字訊息給 LINE 使用者（實作 alert.Notifier 介面）.
func (n *LinePushNotifier) Push(ctx context.Context, userID, text string) error {
	return PushText(ctx, userID, text)
}

// PushText 推播文字訊息給 LINE 使用者.
func PushText(ctx context.Context, userID, text string) error {
	cfg := config.Get()

	push := models.PushMessage{
		To: userID,
		Messages: []models.ReplyBody{
			{Type: "text", Text: text},
		},
	}

	body, err := json.Marshal(push)
	if err != nil {
		return fmt.Errorf("marshal push message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Line.PushURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create push request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Line.ChannelToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send push: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("LINE push API non-200", "status", resp.StatusCode, "userID", userID)
	}

	return nil
}
