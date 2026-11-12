package cwa

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/retry"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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

// Client 氣象署 HTTP 客戶端（F30-31）：每次嘗試各自的期限、只重試暫時性錯誤、回應上限、授權碼只走 header.
type Client struct {
	o ClientOptions
	l *Ledger
	s config.CWASettings
}

// NewClient 建立客戶端；l 可為 nil（不記帳、不判斷額度）.
func NewClient(o ClientOptions, l *Ledger, s config.CWASettings) *Client {
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{}
	}
	if o.AttemptTimeout == 0 {
		o.AttemptTimeout = AlertAttemptTimeout
	}
	if o.Backoff == 0 {
		o.Backoff = RetryBackoff
	}
	if o.MaxBody == 0 {
		o.MaxBody = MaxBody
	}
	if o.Sleep == nil {
		o.Sleep = retry.SleepCtx
	}
	return &Client{o: o, l: l, s: s}
}

// FetchBudget 單次抓取的最壞預算 = MaxAttempts×AttemptTimeout + (MaxAttempts−1)×Backoff.
func (c *Client) FetchBudget() time.Duration {
	n := max(c.s.MaxAttempts, 1)
	return time.Duration(n)*c.o.AttemptTimeout + time.Duration(n-1)*c.o.Backoff
}

// attemptErr 單次嘗試的失敗：分類、是否可重試. 只在套件內流動，不外洩.
type attemptErr struct {
	class     string
	retryable bool
	err       error
}

func (e *attemptErr) Error() string { return "cwa attempt failed: " + e.class }
func (e *attemptErr) Unwrap() error { return e.err }

// GetJSON 打 BaseURL + "/" + path，query 附加；成功（200）把 body 解進 out. 回傳形狀見 D2b：
// 成功 nil；第 1 次嘗試被額度擋下回裸 ErrQuotaReserved；其餘失敗一律 *Error.
func (c *Client) GetJSON(ctx context.Context, spec CallSpec, path string, query url.Values, out any) error {
	target := c.o.BaseURL + "/" + path
	if q := query.Encode(); q != "" {
		target += "?" + q
	}
	var last *attemptErr
	policy := retry.Policy{
		MaxAttempts: c.s.MaxAttempts,
		Backoff:     func(int) time.Duration { return c.o.Backoff },
		Sleep:       c.o.Sleep,
		// 只在父 ctx 仍活著時才可能重試（關機或本輪逾時不重試）.
		Retryable: func(err error) bool {
			var ae *attemptErr
			return ctx.Err() == nil && errors.As(err, &ae) && ae.retryable
		},
		BeforeAttempt: func(ctx context.Context, attempt int) error {
			if c.l == nil {
				return nil
			}
			return c.l.reserve(ctx, spec, attempt)
		},
	}
	attempts, err := retry.Do(ctx, policy, func(ctx context.Context, attempt int) error {
		actx, cancel := context.WithTimeout(ctx, c.o.AttemptTimeout)
		defer cancel()
		aerr := c.once(actx, spec, attempt, target, out)
		if aerr != nil {
			last = aerr
			return aerr
		}
		return nil
	})
	if attempts >= 2 {
		slog.Info("cwa fetch retried", "dataset", spec.Dataset, "attempts", attempts, "ok", err == nil)
	}
	if err == nil {
		return nil
	}
	return c.finalError(ctx, err, last, attempts)
}

// finalError 把 retry.Do 的結果整理成 D2b 的回傳形狀.
func (c *Client) finalError(ctx context.Context, err error, last *attemptErr, attempts int) error {
	if errors.Is(err, ErrQuotaReserved) {
		if attempts == 0 {
			return ErrQuotaReserved
		}
		class := classNetwork
		if last != nil {
			class = last.class
		}
		return &Error{Class: class, Attempts: attempts, Err: ErrQuotaReserved}
	}
	// 父 ctx 取消或到期而終止：只對傳輸層的失敗改記為 timeout / network.
	if cerr := ctx.Err(); cerr != nil && (last == nil || last.retryable) {
		class := classNetwork
		if errors.Is(cerr, context.DeadlineExceeded) {
			class = classTimeout
		}
		return &Error{Class: class, Attempts: attempts, Err: cerr}
	}
	var ae *attemptErr
	if errors.As(err, &ae) {
		return &Error{Class: ae.class, Attempts: attempts, Err: ae.err}
	}
	return &Error{Class: Classify(err), Attempts: attempts, Err: err}
}

// once 一次嘗試. 授權碼只放 header；URL 與任何 log 不含它.
func (c *Client) once(ctx context.Context, spec CallSpec, attempt int, target string, out any) *attemptErr {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return &attemptErr{class: classNetwork, err: errBadRequest}
	}
	req.Header.Set("Authorization", c.o.AuthKey)
	resp, err := c.o.HTTPClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // 去掉 URL.
		}
		return &attemptErr{class: Classify(err), retryable: transient(err), err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if c.l != nil {
		c.l.noteResponse(spec, attempt, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		exhausted := slices.Contains(c.s.ExhaustedStatus, resp.StatusCode)
		return &attemptErr{
			class:     "status_" + strconv.Itoa(resp.StatusCode),
			retryable: resp.StatusCode >= http.StatusInternalServerError && !exhausted,
			err:       errStatus,
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.o.MaxBody+1))
	if err != nil {
		return &attemptErr{class: Classify(err), retryable: true, err: err}
	}
	if int64(len(body)) > c.o.MaxBody {
		return &attemptErr{class: classTooLarge, err: errTooLarge}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &attemptErr{class: classDecode, err: errDecode}
	}
	return nil
}

// 內層原因的固定錯誤（不帶任何伺服器內容）.
var (
	errBadRequest = errors.New("cwa: bad request")
	errStatus     = errors.New("cwa: unexpected status")
	errTooLarge   = errors.New("cwa: response too large")
	errDecode     = errors.New("cwa: decode failed")
)
