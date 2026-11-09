package ai

import (
	"chatbot-go/internal/redact"
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// APIError 供應商回傳的非 2xx 回應.
type APIError struct {
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%d: %s", e.Status, e.Message)
}

var (
	// ErrEmpty 模型回傳空內容.
	ErrEmpty = errors.New("empty response")
	// ErrRejected 回答未通過 Request.Accept 檢查.
	ErrRejected = errors.New("回應格式不符")
)

// 模型暫時不可用的冷卻時間.
const (
	notFoundCooldown   = 24 * time.Hour
	dailyQuotaCooldown = time.Hour
	tooLargeCooldown   = 15 * time.Minute
)

// unusableFor 回傳這個錯誤讓模型在多久內不值得再試；0 表示可以繼續用.
//
// 404 / 模型不存在 / 不支援圖片：這個帳號或這種請求根本叫不動.
// 每日額度用完：短時間內不會恢復.
// 單次請求本身超過每分鐘上限：等多久都不會成功（Groq qwen OTPM 1000 的教訓）.
func unusableFor(err error) time.Duration {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return 0
	}
	msg := strings.ToLower(apiErr.Message)
	switch {
	case apiErr.Status == 404,
		strings.Contains(msg, "model_not_found"),
		strings.Contains(msg, "no longer available"),
		strings.Contains(msg, "does not exist"),
		strings.Contains(msg, "image") && strings.Contains(msg, "not support"):
		return notFoundCooldown
	case strings.Contains(msg, "per day"),
		strings.Contains(msg, "perday"),
		strings.Contains(msg, "requestsperday"):
		return dailyQuotaCooldown
	case strings.Contains(msg, "request too large"):
		return tooLargeCooldown
	}
	return 0
}

var retryInPattern = regexp.MustCompile(`(?i)(?:try again|retry) in ([0-9.]+)\s*s`)

// retryWait 回傳 429 建議的等待時間（Retry-After header、"try again in 25.5s"、"retry in 23s"）.
func retryWait(err error) (time.Duration, bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 429 {
		return 0, false
	}
	if apiErr.RetryAfter > 0 {
		return apiErr.RetryAfter + time.Second, true
	}
	m := retryInPattern.FindStringSubmatch(apiErr.Message)
	if m == nil {
		return 0, false
	}
	seconds, parseErr := strconv.ParseFloat(m[1], 64)
	if parseErr != nil {
		return 0, false
	}
	return time.Duration(seconds*float64(time.Second)) + time.Second, true
}

const maxErrorRunes = 160

// shortError 產生一行已遮蔽的失敗原因.
func shortError(label, model string, err error) string {
	prefix := fmt.Sprintf("%s(%s)", label, model)
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return prefix + ": 逾時無回應"
	}
	// [F24-97] APIError.Message 是供應商回應本文，可能回顯 prompt；摘要只留 HTTP 狀態碼.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return prefix + fmt.Sprintf(" %d", apiErr.Status)
	}
	text := []rune(redact.Error(err))
	if len(text) > maxErrorRunes {
		text = append(text[:maxErrorRunes], '…')
	}
	return prefix + " " + string(text)
}
