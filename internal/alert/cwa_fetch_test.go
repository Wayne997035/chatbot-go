package alert

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fetchServer 前 n 次請求用 first 處理、之後回 001 空特報；記請求數.
func fetchServer(t *testing.T, n int32, first func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= n {
			first(w, r)
			return
		}
		_, _ = w.Write([]byte(noWarnings))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func optClient(o cwa.ClientOptions) *cwa.Client {
	if o.Backoff == 0 {
		o.Backoff = time.Millisecond
	}
	if o.AuthKey == "" {
		o.AuthKey = "test-key"
	}
	return cwa.NewClient(o, nil, (&config.Config{}).CWASettings())
}

// [F30-40] 連線被對方直接關掉（暫時性網路錯誤）→ 重試一次後成功.
func TestFetchRetriesTransientNetworkError(t *testing.T) {
	srv, hits := fetchServer(t, 1, func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	if _, err := FetchWeatherWarnings(context.Background(), optClient(cwa.ClientOptions{BaseURL: srv.URL}), "W-C0033-001"); err != nil {
		t.Fatalf("want success after one retry, got %v", err)
	}
	if hits.Load() != 2 {
		t.Errorf("requests = %d, want 2", hits.Load())
	}
}

// [F30-40] 5xx → 重試一次後成功.
func TestFetchRetriesOn5xx(t *testing.T) {
	srv, hits := fetchServer(t, 1, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	if _, err := FetchWeatherWarnings(context.Background(), optClient(cwa.ClientOptions{BaseURL: srv.URL}), "W-C0033-001"); err != nil {
		t.Fatalf("want success after one retry, got %v", err)
	}
	if hits.Load() != 2 {
		t.Errorf("requests = %d, want 2", hits.Load())
	}
}

// [F30-40] 4xx 不重試：只打一次，分類為 status_<code>.
func TestFetchDoesNotRetryClientErrors(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests} {
		srv, hits := fetchServer(t, 10, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
		_, err := FetchWeatherWarnings(context.Background(), optClient(cwa.ClientOptions{BaseURL: srv.URL}), "W-C0033-001")
		if err == nil || hits.Load() != 1 {
			t.Errorf("status %d: err=%v requests=%d, want error and 1 request", code, err, hits.Load())
		}
		if got, want := cwa.ClassOf(err), fmt.Sprintf("status_%d", code); got != want {
			t.Errorf("status %d: class %q, want %q", code, got, want)
		}
	}
}

// [F30-40] 回應解析失敗不重試（重打也是同一份壞資料）.
func TestFetchDoesNotRetryParseError(t *testing.T) {
	srv, hits := fetchServer(t, 10, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>not json")) })
	_, err := FetchWeatherWarnings(context.Background(), optClient(cwa.ClientOptions{BaseURL: srv.URL}), "W-C0033-001")
	if cwa.ClassOf(err) != "decode" || hits.Load() != 1 {
		t.Errorf("class=%q requests=%d, want decode and 1 request (err=%v)", cwa.ClassOf(err), hits.Load(), err)
	}
}

// [F30-40] 單次嘗試各自有期限：第一次卡住被砍掉，第二次成功.
func TestFetchPerAttemptTimeout(t *testing.T) {
	srv, hits := fetchServer(t, 1, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	start := time.Now()
	_, err := FetchWeatherWarnings(context.Background(),
		optClient(cwa.ClientOptions{BaseURL: srv.URL, AttemptTimeout: 100 * time.Millisecond}), "W-C0033-001")
	if err != nil {
		t.Fatalf("second attempt must succeed after the first timed out: %v", err)
	}
	if hits.Load() != 2 || time.Since(start) > 3*time.Second {
		t.Errorf("requests=%d elapsed=%v", hits.Load(), time.Since(start))
	}
}

// [F30-40] 呼叫端取消（例如關機）後不再重試.
func TestFetchStopsRetryOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, hits := fetchServer(t, 10, func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	start := time.Now()
	_, err := FetchWeatherWarnings(ctx, optClient(cwa.ClientOptions{BaseURL: srv.URL, Backoff: 5 * time.Second}), "W-C0033-001")
	if time.Since(start) > time.Second {
		t.Errorf("returned after %v, must not wait out the 5s backoff", time.Since(start))
	}
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want one wrapping context.Canceled", err)
	}
	if hits.Load() != 1 {
		t.Errorf("requests = %d, want 1 (no retry after cancel)", hits.Load())
	}
}

// [F30-40] 重試有一行 Info log，帶資料集、次數與結果.
func TestRetryLogFields(t *testing.T) {
	logs := captureText(t)
	srv, _ := fetchServer(t, 1, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	if _, err := FetchWeatherWarnings(context.Background(), optClient(cwa.ClientOptions{BaseURL: srv.URL}), "W-C0033-001"); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	for _, want := range []string{"cwa fetch retried", "dataset=W-C0033-001", "attempts=2", "ok=true"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q: %s", want, out)
		}
	}
	for _, bad := range []string{"http://", "?format="} {
		if strings.Contains(out, bad) {
			t.Errorf("log leaks %q: %s", bad, out)
		}
	}

	// 一次就成功的路徑不印這行.
	quiet := captureText(t)
	okSrv, _ := fetchServer(t, 0, nil)
	if _, err := FetchWeatherWarnings(context.Background(), optClient(cwa.ClientOptions{BaseURL: okSrv.URL}), "W-C0033-001"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(quiet.String(), "cwa fetch retried") {
		t.Errorf("single-attempt success logged a retry: %s", quiet.String())
	}
}

// [F30-40] 授權碼只走 header：不在 URL、不在錯誤、不在 log.
func TestAuthKeyNeverInURLOrLogs(t *testing.T) {
	const key = "SECRET-AUTH-KEY-123"
	logs := captureText(t)
	var badURL, badHeader atomic.Int32
	srv, _ := fetchServer(t, 10, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.String(), key) {
			badURL.Add(1)
		}
		if r.Header.Get("Authorization") != key {
			badHeader.Add(1)
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, err := FetchWeatherWarnings(context.Background(), optClient(cwa.ClientOptions{BaseURL: srv.URL, AuthKey: key}), "W-C0033-001")
	if err == nil {
		t.Fatal("want an error")
	}
	if badURL.Load() != 0 || badHeader.Load() != 0 {
		t.Errorf("key in URL %d times, missing from header %d times", badURL.Load(), badHeader.Load())
	}
	if strings.Contains(err.Error(), key) || strings.Contains(logs.String(), key) {
		t.Errorf("key leaked: err=%v logs=%s", err, logs.String())
	}
}

// [F30-40] 回應超過上限 → too_large，不重試.
func TestFetchBodyLimit(t *testing.T) {
	srv, hits := fetchServer(t, 10, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":"` + strings.Repeat("a", 4096) + `"}`))
	})
	_, err := FetchWeatherWarnings(context.Background(), optClient(cwa.ClientOptions{BaseURL: srv.URL, MaxBody: 256}), "W-C0033-001")
	if cwa.ClassOf(err) != "too_large" || hits.Load() != 1 {
		t.Errorf("class=%q requests=%d, want too_large and 1 request (err=%v)", cwa.ClassOf(err), hits.Load(), err)
	}

	// 恰等於上限的回應仍成功（邊界）.
	const limit = 256
	exact := `{"success":"` + strings.Repeat("a", limit-len(`{"success":""}`)) + `"}`
	edge, _ := fetchServer(t, 10, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(exact)) })
	if len(exact) != limit {
		t.Fatalf("fixture length %d, want %d", len(exact), limit)
	}
	if _, err := FetchWeatherWarnings(context.Background(), optClient(cwa.ClientOptions{BaseURL: edge.URL, MaxBody: limit}), "W-C0033-001"); err != nil {
		t.Errorf("a body exactly at the limit must succeed: %v", err)
	}
}

// [F30-40] 單次嘗試逾時且呼叫端的 ctx 還活著 → 視為暫時性，重試；兩次都逾時才回 timeout.
func TestAttemptTimeoutIsRetriedWhenParentAlive(t *testing.T) {
	srv, hits := fetchServer(t, 10, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	_, err := FetchWeatherWarnings(context.Background(),
		optClient(cwa.ClientOptions{BaseURL: srv.URL, AttemptTimeout: 50 * time.Millisecond}), "W-C0033-001")
	var ce *cwa.Error
	if !errors.As(err, &ce) || ce.Class != "timeout" || ce.Attempts != 2 || hits.Load() != 2 {
		t.Errorf("err=%v requests=%d, want *cwa.Error timeout after 2 attempts", err, hits.Load())
	}
}

// [F30-40] 呼叫端的期限到了（本輪逾時）不重試：只打一次，分類 timeout.
func TestParentDeadlineIsNotRetried(t *testing.T) {
	srv, hits := fetchServer(t, 10, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err := FetchWeatherWarnings(ctx, optClient(cwa.ClientOptions{BaseURL: srv.URL}), "W-C0033-001")
	if cwa.ClassOf(err) != "timeout" || !errors.Is(err, context.DeadlineExceeded) || hits.Load() != 1 {
		t.Errorf("class=%q requests=%d err=%v, want timeout wrapping DeadlineExceeded after 1 request", cwa.ClassOf(err), hits.Load(), err)
	}
}

// [F30-40] 連不上時的錯誤字串不含位址、query、主機名與內層原文.
func TestFetchErrorsCarryNoServerAddress(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // 之後連這個位址 = connection refused
	_, err := FetchWeatherWarnings(context.Background(), optClient(cwa.ClientOptions{BaseURL: url}), "W-C0033-001")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.HasPrefix(err.Error(), "cwa fetch failed: ") {
		t.Errorf("error %q is not the typed cwa error shape", err.Error())
	}
	for _, bad := range []string{"127.0.0.1", "http://", "?format=", "opendata.cwa.gov.tw", "connection refused", "W-C0033", `Get "`} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("error %q leaks %q", err.Error(), bad)
		}
	}
}

// [F30-40] 每次抓取各自有預算：低溫（004）把自己的預算吃光，後面的總表（001）照樣能抓.
func TestCheckerFetchBudgetsAreIndependent(t *testing.T) {
	feed := newRouteFeed(t)
	feed.hang["W-C0033-004"] = true
	c := newFeedChecker(t, feed, weatherSubs(map[string]string{"U1": "台北市"}), newNotifier(), nil)
	// 單次嘗試 100ms、重試等 10ms → 一次抓取預算 210ms.
	c.WithCWAClient(optClient(cwa.ClientOptions{
		BaseURL: feed.srv.URL, AttemptTimeout: 100 * time.Millisecond, Backoff: 10 * time.Millisecond,
	}))
	if err := c.CheckWarnings(context.Background()); err != nil {
		t.Fatalf("004 burning its own budget must not starve 001: %v", err)
	}
	if feed.count("W-C0033-004") < 1 || feed.count("W-C0033-001") != 1 {
		t.Errorf("004 requests=%d 001 requests=%d", feed.count("W-C0033-004"), feed.count("W-C0033-001"))
	}

	// 總表（001）逾時不影響地震：地震照推，CheckAndNotify 的錯誤只含特報.
	feed2 := newRouteFeed(t)
	feed2.hang["W-C0033-001"] = true
	feed2.set("E-A0015-001", quakeBody(quakeSpec{115001, "2026-11-02 20:40:00"}))
	n := newNotifier()
	c2 := newFeedChecker(t, feed2, weatherAndQuakeSubs(), n, nil)
	c2.WithCWAClient(optClient(cwa.ClientOptions{
		BaseURL: feed2.srv.URL, AttemptTimeout: 100 * time.Millisecond, Backoff: 10 * time.Millisecond,
	}))
	err := c2.CheckAndNotify(context.Background())
	if err == nil || !strings.Contains(err.Error(), "weather warnings") || strings.Contains(err.Error(), "earthquakes") {
		t.Errorf("err = %v, want only the weather warnings failure", err)
	}
	if n.count("U1", "[地震報告]") != 1 {
		t.Errorf("the earthquake must still be pushed when 001 times out: %v", n.pushes)
	}
}
