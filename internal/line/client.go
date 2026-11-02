// Package line LINE Messaging API 用戶端：回覆、推播、下載使用者傳的圖片、輸入中動畫.
// 金鑰與網址都在建立時傳入，不讀全域設定；錯誤訊息一律先遮蔽再回傳.
package line

import (
	"bytes"
	"chatbot-go/internal/models"
	"chatbot-go/internal/redact"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Config 呼叫 LINE API 需要的設定.
type Config struct {
	Token      string
	ReplyURL   string
	PushURL    string
	ContentURL string // 下載圖片，後面接 /{messageId}/content
	LoadingURL string
}

// Client LINE Messaging API 用戶端，可同時給多個 goroutine 使用.
type Client struct {
	cfg  Config
	http *http.Client
}

// DefaultTimeout 單次呼叫的逾時.
const DefaultTimeout = 30 * time.Second

// NewClient 建立用戶端；httpClient 為 nil 時用 DefaultTimeout 的預設 client.
func NewClient(cfg *Config, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{cfg: *cfg, http: httpClient}
}

// Reply 回覆一到五則訊息. 失敗會在這裡統一記錄（reply token 過期、訊息超過限制、429 等），呼叫端不必再各自記錄.
func (c *Client) Reply(ctx context.Context, replyToken string, messages ...any) error {
	err := c.post(ctx, c.cfg.ReplyURL, map[string]any{"replyToken": replyToken, "messages": messages})
	if err != nil {
		slog.Warn("LINE reply failed", "error", err)
	}
	return err
}

// ReplyText 回覆文字訊息；可附 Quick Reply 按鈕.
func (c *Client) ReplyText(ctx context.Context, replyToken, text string, items ...models.QuickReplyItem) error {
	return c.Reply(ctx, replyToken, models.NewText(text, items...))
}

// ReplyFlex 回覆 Flex Message.
func (c *Client) ReplyFlex(ctx context.Context, replyToken, altText string, contents any) error {
	return c.Reply(ctx, replyToken, models.FlexMessage{Type: "flex", AltText: altText, Contents: contents})
}

// Push 推播文字訊息（實作 alert.Notifier）；失敗回傳錯誤，讓呼叫端記錄與重試.
func (c *Client) Push(ctx context.Context, userID, text string) error {
	return c.post(ctx, c.cfg.PushURL, map[string]any{"to": userID, "messages": []any{models.NewText(text)}})
}

// StartLoading 在一對一聊天顯示「輸入中」動畫（best-effort，失敗不影響流程）.
func (c *Client) StartLoading(ctx context.Context, userID string, seconds int) error {
	return c.post(ctx, c.cfg.LoadingURL, map[string]any{"chatId": userID, "loadingSeconds": seconds})
}

// MaxImageBytes 下載圖片的上限（LINE 圖片最大 10MB）.
const MaxImageBytes = 10 << 20

// ErrNotImage 下載到的內容不是圖片.
var ErrNotImage = errors.New("content is not an image")

// DownloadImage 下載使用者傳來的圖片，回傳內容與 MIME 類型.
func (c *Client) DownloadImage(ctx context.Context, messageID string) (data []byte, mime string, err error) {
	url := strings.TrimRight(c.cfg.ContentURL, "/") + "/" + messageID + "/content"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, "", fmt.Errorf("create content request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download content: %s", redact.Error(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("LINE content API returned %d", resp.StatusCode)
	}

	data, err = io.ReadAll(io.LimitReader(resp.Body, MaxImageBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read content: %w", err)
	}
	if len(data) > MaxImageBytes {
		return nil, "", fmt.Errorf("image larger than %d bytes", MaxImageBytes)
	}
	mime = http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return nil, "", ErrNotImage
	}
	return data, mime, nil
}

// post 呼叫 LINE API；非 200 回傳錯誤（訊息已遮蔽）.
func (c *Client) post(ctx context.Context, url string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal LINE payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create LINE request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)

	resp, err := c.http.Do(req)
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
