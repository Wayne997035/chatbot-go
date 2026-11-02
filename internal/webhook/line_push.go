package webhook

import (
	"chatbot-go/internal/models"
	"chatbot-go/internal/platform/config"
	"context"
)

// LinePushNotifier LINE Push 推播實作.
type LinePushNotifier struct{}

// Push 推播文字訊息給 LINE 使用者（實作 alert.Notifier 介面）.
func (n *LinePushNotifier) Push(ctx context.Context, userID, text string) error {
	return PushText(ctx, userID, text)
}

// PushText 推播文字訊息給 LINE 使用者；失敗會回傳錯誤，讓呼叫端記錄.
func PushText(ctx context.Context, userID, text string) error {
	return postLINE(ctx, config.Get().Line.PushURL, map[string]any{"to": userID, "messages": []any{models.NewText(text)}})
}
