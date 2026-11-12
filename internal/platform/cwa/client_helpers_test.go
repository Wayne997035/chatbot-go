package cwa

import (
	"chatbot-go/internal/platform/config"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const testAuthKey = "test-key-SECRET"

// testServer 計數請求並記錄 header 的 httptest 伺服器.
type testServer struct {
	*httptest.Server
	reqs     atomic.Int32
	lastAuth atomic.Value // string
	lastURL  atomic.Value // string
}

func newTestServer(t *testing.T, h func(n int, w http.ResponseWriter, r *http.Request)) *testServer {
	t.Helper()
	ts := &testServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(ts.reqs.Add(1))
		ts.lastAuth.Store(r.Header.Get("Authorization"))
		ts.lastURL.Store(r.URL.String())
		h(n, w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (ts *testServer) count() int { return int(ts.reqs.Load()) }

// hijackClose 不回應直接關閉連線（模擬連線被重置 / EOF）.
func hijackClose(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func okJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// blockUntilDone 讓請求卡住直到客戶端放棄（避免測試伺服器掛住）.
func blockUntilDone(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(3 * time.Second):
	}
}

func newTestClient(ts *testServer, l *Ledger, s *config.CWASettings, mutate func(*ClientOptions)) *Client {
	o := ClientOptions{
		BaseURL: ts.URL, AuthKey: testAuthKey, AttemptTimeout: 2 * time.Second, Backoff: time.Millisecond,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	if mutate != nil {
		mutate(&o)
	}
	return NewClient(o, l, s)
}

type decoded struct {
	OK bool `json:"ok"`
}
