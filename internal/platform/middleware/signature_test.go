package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

// fakeClock 可前進的假時鐘；起點刻意不是 Unix 0（localcache 的 Warn 限流以「上次時間 == 0」判第一筆）.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// warnCapture 擷取 slog Warn 的 handler.
type warnCapture struct {
	mu    sync.Mutex
	warns int
}

func (h *warnCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h *warnCapture) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *warnCapture) WithGroup(string) slog.Handler            { return h }

//nolint:gocritic // slog.Handler interface requires slog.Record by value.
func (h *warnCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.mu.Lock()
		h.warns++
		h.mu.Unlock()
	}
	return nil
}

func (h *warnCapture) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.warns
}

func captureWarns(t *testing.T) *warnCapture {
	t.Helper()
	h := &warnCapture{}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(old) })
	return h
}

func useReplayCache(t *testing.T, c *localLineReplayCache) {
	t.Helper()
	original := lineReplayCache
	lineReplayCache = c
	t.Cleanup(func() { lineReplayCache = original })
}

func sign(body []byte) string {
	const secret = "s3cret"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// [F24-53] TestReplayDistinctBodiesNotBlocked 守住對應行為.
func TestReplayDistinctBodiesNotBlocked(t *testing.T) {
	c := newLocalLineReplayCacheWithClock(newFakeClock().now)
	for i := range 5 {
		if !c.record(lineWebhookReplayKey([]byte(fmt.Sprintf(`{"n":%d}`, i)))) {
			t.Errorf("body %d reported as replay", i)
		}
	}
}

// [F24-53] TestReplayExpiresAfterTTL 守住對應行為.
func TestReplayExpiresAfterTTL(t *testing.T) {
	clk := newFakeClock()
	c := newLocalLineReplayCacheWithClock(clk.now)
	key := lineWebhookReplayKey([]byte(`{"a":1}`))
	if !c.record(key) {
		t.Fatal("first record must pass")
	}
	if c.record(key) {
		t.Fatal("second record inside TTL must be a replay")
	}
	clk.advance(lineWebhookReplayTTL + time.Second)
	if !c.record(key) {
		t.Fatal("record after TTL must pass again")
	}
}

// [F24-53] TestReplayWithinTTLIsRejected 守住對應行為.
func TestReplayWithinTTLIsRejected(t *testing.T) {
	clk := newFakeClock()
	c := newLocalLineReplayCacheWithClock(clk.now)
	key := lineWebhookReplayKey([]byte(`{"a":2}`))
	if !c.record(key) {
		t.Fatal("first record must pass")
	}
	clk.advance(4*time.Minute + 59*time.Second)
	if c.record(key) {
		t.Fatal("record at 4m59s must still be a replay")
	}
}

// [F24-53] TestReplayConcurrentSameBodyOnlyOnce 守住對應行為.
func TestReplayConcurrentSameBodyOnlyOnce(t *testing.T) {
	c := newLocalLineReplayCacheWithClock(newFakeClock().now)
	key := lineWebhookReplayKey([]byte(`{"a":3}`))
	var firsts atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if c.record(key) {
				firsts.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := firsts.Load(); got != 1 {
		t.Fatalf("first-time results = %d, want exactly 1", got)
	}
}

// [F24-53] 寫入 10 倍 warnAbove 筆（假時鐘不前進）後逐 key 重查，全部仍是 replay，且有 Warn.
func TestReplayNeverEvictsBeyondWarnAbove(t *testing.T) {
	h := captureWarns(t)
	c := newLocalLineReplayCacheWithClock(newFakeClock().now)
	n := 10 * lineReplayWarnAbove

	if !c.record("first") {
		t.Fatal("first record must pass")
	}
	for i := range n {
		if !c.record(fmt.Sprintf("k%d", i)) {
			t.Fatalf("k%d reported as replay on first write", i)
		}
	}
	if c.record("first") {
		t.Error("first was evicted: replay would pass")
	}
	for i := range n {
		if c.record(fmt.Sprintf("k%d", i)) {
			t.Fatalf("k%d was evicted or rejected: replay would pass", i)
		}
	}
	if h.count() < 1 {
		t.Error("want >=1 Warn once above lineReplayWarnAbove, got none")
	}
}

func doWebhook(t *testing.T, mw echo.MiddlewareFunc, body []byte, sig string, calls *atomic.Int64) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Line-Signature", sig)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)
	h := mw(func(c echo.Context) error {
		calls.Add(1)
		return c.NoContent(http.StatusOK)
	})
	if err := h(ctx); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return rec
}

// [F24-53] TestLineSignatureValidatorRejectsReplayWith409 守住對應行為.
func TestLineSignatureValidatorRejectsReplayWith409(t *testing.T) {
	useReplayCache(t, newLocalLineReplayCache())
	const secret = "s3cret"
	mw := LineSignatureValidator(secret, nil)
	body := []byte(`{"events":[{"webhookEventId":"01MW"}]}`)
	var calls atomic.Int64

	if rec := doWebhook(t, mw, body, sign(body), &calls); rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", rec.Code)
	}
	rec := doWebhook(t, mw, body, sign(body), &calls)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Duplicate webhook request") {
		t.Errorf("409 body = %q, want Duplicate webhook request", rec.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("handler calls = %d, want 1", got)
	}
}

// [F24-53] 壞簽名不得佔防重放名額.
func TestLineSignatureValidatorBadSignatureDoesNotConsumeReplaySlot(t *testing.T) {
	useReplayCache(t, newLocalLineReplayCache())
	const secret = "s3cret"
	mw := LineSignatureValidator(secret, nil)
	body := []byte(`{"events":[{"webhookEventId":"01BAD"}]}`)
	var calls atomic.Int64

	for i := range 2 {
		if rec := doWebhook(t, mw, body, "bad-signature", &calls); rec.Code != http.StatusUnauthorized {
			t.Fatalf("bad signature #%d status = %d, want 401", i+1, rec.Code)
		}
	}
	if rec := doWebhook(t, mw, body, sign(body), &calls); rec.Code != http.StatusOK {
		t.Fatalf("valid signature after bad ones status = %d, want 200", rec.Code)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("handler calls = %d, want 1", got)
	}
}

func TestValidateHMAC(t *testing.T) {
	secret := "test-secret-key"
	body := []byte(`{"events":[]}`)

	// 計算正確簽名
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	validSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	tests := []struct {
		name      string
		body      []byte
		signature string
		secret    string
		want      bool
	}{
		{"valid signature", body, validSig, secret, true},
		{"wrong signature", body, "wrong-signature", secret, false},
		{"wrong secret", body, validSig, "wrong-secret", false},
		{"empty body", []byte{}, validSig, secret, false},
		{"empty signature", body, "", secret, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateHMAC(tt.body, tt.signature, tt.secret)
			if got != tt.want {
				t.Errorf("validateHMAC() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRecordLineWebhookBodyRejectsReplayWithLocalCache(t *testing.T) {
	originalCache := lineReplayCache
	lineReplayCache = newLocalLineReplayCache()
	t.Cleanup(func() {
		lineReplayCache = originalCache
	})

	body := []byte(`{"events":[{"webhookEventId":"01TEST"}]}`)
	if err := recordLineWebhookBody(context.Background(), nil, body); err != nil {
		t.Fatalf("recordLineWebhookBody() first call error = %v", err)
	}
	if err := recordLineWebhookBody(context.Background(), nil, body); !errors.Is(err, errLineWebhookReplay) {
		t.Fatalf("recordLineWebhookBody() second call error = %v, want replay", err)
	}
}

// Redis 中途斷線時不能拒收 LINE webhook（LINE 預設不重送，拒收就是訊息遺失）；改用記憶體防重放.
func TestReplayCheckFallsBackToMemoryWhenRedisFails(t *testing.T) {
	originalCache := lineReplayCache
	lineReplayCache = newLocalLineReplayCache()
	t.Cleanup(func() { lineReplayCache = originalCache })

	// 連到沒有服務的 port：每次操作都會失敗.
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = down.Close() })

	body := []byte(`{"events":[{"webhookEventId":"01DOWN"}]}`)
	if err := recordLineWebhookBody(context.Background(), down, body); err != nil {
		t.Fatalf("a Redis failure must not reject the webhook: %v", err)
	}
	if err := recordLineWebhookBody(context.Background(), down, body); !errors.Is(err, errLineWebhookReplay) {
		t.Errorf("replays are still caught in memory, got %v", err)
	}
}

func TestReadLineWebhookBody(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantLen int64
		wantErr error
	}{
		{
			name:    "within limit",
			body:    `{"events":[]}`,
			wantLen: int64(len(`{"events":[]}`)),
		},
		{
			name:    "exactly at limit",
			body:    strings.Repeat("a", int(maxLineWebhookBodyBytes)),
			wantLen: maxLineWebhookBodyBytes,
		},
		{
			name:    "over limit",
			body:    strings.Repeat("a", int(maxLineWebhookBodyBytes)+1),
			wantErr: errLineWebhookBodyTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readLineWebhookBody(strings.NewReader(tt.body))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("readLineWebhookBody() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("readLineWebhookBody() error = %v", err)
			}
			if int64(len(got)) != tt.wantLen {
				t.Fatalf("readLineWebhookBody() len = %d, want %d", len(got), tt.wantLen)
			}
		})
	}
}

// sendWebhook 用指定的 handler 送一次 webhook，回傳狀態碼.
func sendWebhook(t *testing.T, mw echo.MiddlewareFunc, body []byte, handler echo.HandlerFunc) int {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Line-Signature", sign(body))
	rec := httptest.NewRecorder()
	if err := mw(handler)(e.NewContext(req, rec)); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return rec.Code
}

// [F30-67] TestBusy503ForgetsReplayRecord 守住：handler 回 503（忙碌、還沒接手）後，同一個 body 重送不被防重放擋；其他狀態碼不撤回.
func TestBusy503ForgetsReplayRecord(t *testing.T) {
	const secret = "s3cret"
	respond := func(codes ...int) (echo.HandlerFunc, *atomic.Int64) {
		var n atomic.Int64
		return func(c echo.Context) error {
			i := int(n.Add(1)) - 1
			return c.NoContent(codes[min(i, len(codes)-1)])
		}, &n
	}
	backends := []struct {
		name   string
		client func(t *testing.T) *redis.Client
	}{
		{"memory", func(*testing.T) *redis.Client { return nil }},
		{"redis", func(t *testing.T) *redis.Client {
			mr := miniredis.RunT(t)
			c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = c.Close() })
			return c
		}},
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Run("503 then resend reaches handler", func(t *testing.T) {
				useReplayCache(t, newLocalLineReplayCache())
				mw := LineSignatureValidator(secret, b.client(t))
				h, calls := respond(http.StatusServiceUnavailable, http.StatusOK)
				body := []byte(`{"events":[{"webhookEventId":"01BUSY-` + b.name + `"}]}`)
				if got := sendWebhook(t, mw, body, h); got != http.StatusServiceUnavailable {
					t.Fatalf("first status = %d, want 503", got)
				}
				if got := sendWebhook(t, mw, body, h); got != http.StatusOK {
					t.Fatalf("resend after 503 status = %d, want 200 (not 409)", got)
				}
				if calls.Load() != 2 {
					t.Errorf("handler calls = %d, want 2", calls.Load())
				}
				// 對照：200 之後同 body 再送仍是 409.
				if got := sendWebhook(t, mw, body, h); got != http.StatusConflict {
					t.Errorf("replay after 200 status = %d, want 409", got)
				}
			})
			t.Run("500 is not forgotten", func(t *testing.T) {
				useReplayCache(t, newLocalLineReplayCache())
				mw := LineSignatureValidator(secret, b.client(t))
				h, calls := respond(http.StatusInternalServerError, http.StatusOK)
				body := []byte(`{"events":[{"webhookEventId":"01ERR-` + b.name + `"}]}`)
				if got := sendWebhook(t, mw, body, h); got != http.StatusInternalServerError {
					t.Fatalf("first status = %d, want 500", got)
				}
				if got := sendWebhook(t, mw, body, h); got != http.StatusConflict {
					t.Errorf("resend after 500 status = %d, want 409 (may be half processed)", got)
				}
				if calls.Load() != 1 {
					t.Errorf("handler calls = %d, want 1", calls.Load())
				}
			})
		})
	}
}

// [F30-67] TestReplayStillRejectedWhileFirstIsAccepted 守住：第一個請求還在處理時，同 body 的第二個請求照樣 409.
func TestReplayStillRejectedWhileFirstIsAccepted(t *testing.T) {
	useReplayCache(t, newLocalLineReplayCache())
	const secret = "s3cret"
	mw := LineSignatureValidator(secret, nil)
	body := []byte(`{"events":[{"webhookEventId":"01INFLIGHT"}]}`)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan int, 1)
	go func() {
		done <- sendWebhook(t, mw, body, func(c echo.Context) error {
			close(entered)
			<-release
			return c.NoContent(http.StatusOK)
		})
	}()
	<-entered
	var calls atomic.Int64
	if rec := doWebhook(t, mw, body, sign(body), &calls); rec.Code != http.StatusConflict {
		t.Errorf("second status while first in flight = %d, want 409", rec.Code)
	}
	close(release)
	if got := <-done; got != http.StatusOK {
		t.Errorf("first status = %d, want 200", got)
	}
	if calls.Load() != 0 {
		t.Errorf("second handler calls = %d, want 0", calls.Load())
	}
}
