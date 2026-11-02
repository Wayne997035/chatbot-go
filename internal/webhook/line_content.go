package webhook

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/redact"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxImageBytes 下載圖片的上限（LINE 圖片最大 10MB）.
const maxImageBytes = 10 << 20

var errNotImage = errors.New("content is not an image")

// DownloadImage 下載使用者傳來的圖片，回傳內容與 MIME 類型.
func DownloadImage(ctx context.Context, messageID string) (data []byte, mime string, err error) {
	cfg := config.Get()
	url := strings.TrimRight(cfg.Line.ContentURL, "/") + "/" + messageID + "/content"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, "", fmt.Errorf("create content request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Line.ChannelToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download content: %s", redact.Error(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("LINE content API returned %d", resp.StatusCode)
	}

	data, err = io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read content: %w", err)
	}
	if len(data) > maxImageBytes {
		return nil, "", fmt.Errorf("image larger than %d bytes", maxImageBytes)
	}
	mime = http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return nil, "", errNotImage
	}
	return data, mime, nil
}

// StartLoading 在一對一聊天顯示「輸入中」動畫（best-effort，失敗不影響流程）.
func StartLoading(ctx context.Context, userID string, seconds int) error {
	return postLINE(ctx, config.Get().Line.LoadingURL, map[string]any{"chatId": userID, "loadingSeconds": seconds})
}
