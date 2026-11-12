package cwa

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// [F30-82] 憑證錯誤（TLS 憑證不受信任）不重試：只嘗試一次、不睡、記帳一次.
func TestCertificateErrorIsNotRetried(t *testing.T) {
	var handled atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		handled.Add(1)
		okJSON(w)
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // 握手失敗的伺服器端 log 不必進測試輸出.
	srv.StartTLS()
	t.Cleanup(srv.Close)

	s := testSettings(nil)
	if s.MaxAttempts != 2 {
		t.Fatalf("test assumes default MaxAttempts 2, got %d", s.MaxAttempts)
	}
	var sleeps atomic.Int32
	ledger := NewLedger(nil, s, taipeiLoc(t), newTestClock(t).Now)
	// 不給 HTTPClient：用預設客戶端，它不信任 httptest 的自簽憑證.
	c := NewClient(ClientOptions{
		BaseURL: srv.URL,
		AuthKey: testAuthKey,
		Backoff: time.Millisecond,
		Sleep:   func(context.Context, time.Duration) error { sleeps.Add(1); return nil },
	}, ledger, s)

	var out map[string]any
	err := c.GetJSON(context.Background(), CallSpec{Stream: StreamWarning, Dataset: "W-C0033-001"}, "W-C0033-001", nil, &out)

	if err == nil {
		t.Fatal("want an error for an untrusted certificate")
	}
	if got := sleeps.Load(); got != 0 {
		t.Errorf("Sleep called %d times, want 0 (certificate errors must not be retried)", got)
	}
	if got := ledger.Usage(context.Background()).Total; got != 1 {
		t.Errorf("Usage().Total = %d, want 1 attempt", got)
	}
	if got := handled.Load(); got != 0 {
		t.Errorf("handler called %d times, want 0 (TLS verification fails before any request)", got)
	}
}

// [F30-85] 客戶端與記帳建好後，呼叫端再改那份設定，不影響它們（建構時複製值）.
func TestClientAndLedgerCopySettings(t *testing.T) {
	s := *testSettings(nil)
	if s.MaxAttempts == 1 || s.DailyLimit == 1 {
		t.Fatalf("test assumes defaults differ from 1: MaxAttempts=%d DailyLimit=%d", s.MaxAttempts, s.DailyLimit)
	}
	c := NewClient(ClientOptions{BaseURL: "http://unused.invalid", AuthKey: testAuthKey}, nil, &s)
	l := NewLedger(nil, &s, taipeiLoc(t), newTestClock(t).Now)
	wantBudget := c.FetchBudget()
	wantLimit := l.Usage(context.Background()).Limit

	s.MaxAttempts = 1
	s.DailyLimit = 1

	if got := c.FetchBudget(); got != wantBudget {
		t.Errorf("FetchBudget changed from %v to %v after the caller edited its settings", wantBudget, got)
	}
	if got := l.Usage(context.Background()).Limit; got != wantLimit {
		t.Errorf("Ledger Limit changed from %d to %d after the caller edited its settings", wantLimit, got)
	}
}
