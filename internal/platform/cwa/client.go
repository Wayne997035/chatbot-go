package cwa

import (
	"chatbot-go/internal/platform/config"
	"context"
	"net/http"
	"net/url"
	"time"
)

// ClientOptions 客戶端選項.
type ClientOptions struct {
	BaseURL        string
	AuthKey        string
	HTTPClient     *http.Client                                     // nil → &http.Client{}（不設 Timeout，由每次嘗試的 ctx 期限管）.
	AttemptTimeout time.Duration                                    // 0 → AlertAttemptTimeout.
	Backoff        time.Duration                                    // 0 → RetryBackoff.
	MaxBody        int64                                            // 0 → MaxBody.
	Sleep          func(ctx context.Context, d time.Duration) error // nil → retry.SleepCtx.
}

// Client 氣象署 HTTP 客戶端（F30-31）. 契約階段 GetJSON 為空殼.
type Client struct {
	o ClientOptions
	l *Ledger
	s config.CWASettings
}

// NewClient 建立客戶端.
func NewClient(o ClientOptions, l *Ledger, s config.CWASettings) *Client {
	if o.AttemptTimeout == 0 {
		o.AttemptTimeout = AlertAttemptTimeout
	}
	if o.Backoff == 0 {
		o.Backoff = RetryBackoff
	}
	return &Client{o: o, l: l, s: s}
}

// GetJSON 打 BaseURL + "/" + path，query 原樣附加；成功（200）把 body 解進 out. 契約空殼：一律回錯.
func (c *Client) GetJSON(_ context.Context, _ CallSpec, _ string, _ url.Values, _ any) error {
	return errContractStub
}

// FetchBudget 單次抓取的最壞預算 = MaxAttempts×AttemptTimeout + (MaxAttempts−1)×Backoff.
func (c *Client) FetchBudget() time.Duration {
	n := max(c.s.MaxAttempts, 1)
	return time.Duration(n)*c.o.AttemptTimeout + time.Duration(n-1)*c.o.Backoff
}
