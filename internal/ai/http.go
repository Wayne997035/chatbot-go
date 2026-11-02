package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const maxResponseBytes = 4 << 20

// doJSON 送出 JSON 請求並解析 JSON 回應；非 2xx 回傳 *APIError.
func doJSON(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body, out any) error {
	var reader io.Reader = http.NoBody
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Status: resp.StatusCode, Message: errorMessage(data), RetryAfter: retryAfterHeader(resp.Header.Get("Retry-After"))}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// errorMessage 從各家錯誤格式取出訊息：{"error":{"message":...}}、{"error":"..."}、{"detail":...}.
func errorMessage(data []byte) string {
	var payload struct {
		Error  json.RawMessage `json:"error"`
		Detail any             `json:"detail"`
	}
	if json.Unmarshal(data, &payload) == nil {
		var nested struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(payload.Error, &nested) == nil && nested.Message != "" {
			return nested.Message
		}
		var plain string
		if json.Unmarshal(payload.Error, &plain) == nil && plain != "" {
			return plain
		}
		if payload.Detail != nil {
			return fmt.Sprint(payload.Detail)
		}
	}
	const maxRaw = 500
	if len(data) > maxRaw {
		data = data[:maxRaw]
	}
	return string(data)
}

func retryAfterHeader(v string) time.Duration {
	if v == "" {
		return 0
	}
	seconds, err := strconv.ParseFloat(v, 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}
