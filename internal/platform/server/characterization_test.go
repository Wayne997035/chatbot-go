package server

import (
	"chatbot-go/internal/redact"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

// [F34-78] 這個檔案在 echo v4 上寫成、v4 上綠，升級 v5 後預期值一個字都不改：它鎖的是「行為不變」.
// 只有下面的 harness 區（啟動 / 取實例的方式）允許隨 API 改動.

// ---- harness（可隨 echo API 改動；預期值不可）----

// startRunning 以 serve 啟動 e 並等到可連線，回傳實際跑起來的 http.Server 與位址；stop 用來通知 serve 結束.
func startRunning(t *testing.T, e *echo.Echo) (srv *http.Server, addr string, stop chan struct{}) {
	t.Helper()
	stop = make(chan struct{})
	// v5 沒有 e.Server：實例取自唯一的建構點 newHTTPServer，交給 serve 跑起來之後才被斷言.
	srv = newHTTPServer(e, "127.0.0.1:0")
	ln, err := listen(context.Background(), srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- serve(srv, ln, stop) }()
	t.Cleanup(func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		<-served
		_ = srv.Close()
	})
	return srv, ln.Addr().String(), stop
}

// shutdownRunning 呼叫 shutdown（簽名隨 API 改動）.
func shutdownRunning(ctx context.Context, srv *http.Server, stoppers []Stopper) error {
	return shutdown(ctx, srv, stoppers)
}

// handleFunc 註冊測試用路由.
func handleFunc(e *echo.Echo, method, path string, h func(c *echo.Context) error) {
	e.Add(method, path, h)
}

// ---- helpers ----

func serveOnce(e *echo.Echo, method, target string, body io.Reader, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, target, body)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// serveRecovering 同 serveOnce，但沒被 recover middleware 接住的 panic 會轉成測試失敗（具名斷言），不讓測試行程崩潰.
func serveRecovering(t *testing.T, e *echo.Echo, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic escaped the middleware chain (recover middleware missing?): %v", r)
		}
	}()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, target, http.NoBody))
	return rec
}

// [F34-70] go.mod 只剩 labstack/echo/v5（≥ 5.4.0），v4 不得殘留；import 與 Context 簽名的遷移由整包編譯守住.
func TestEchoModuleIsV5(t *testing.T) {
	raw, err := os.ReadFile("../../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	mod := string(raw)
	// 舊版模組路徑的字面拆開寫，讓全 repo 搜尋舊 import 路徑時零命中.
	if oldModule := "labstack/echo/" + "v4"; strings.Contains(mod, oldModule) {
		t.Errorf("go.mod still requires %s", oldModule)
	}
	if !strings.Contains(mod, "github.com/labstack/echo/v5 v5.") {
		t.Error("go.mod does not require github.com/labstack/echo/v5")
	}
}

// 404 / 405 / 400 / 413 的狀態碼、Content-Type 與 body 逐位元（含 message 文字）.
func TestCharacterizeErrorResponsesByteForByte(t *testing.T) {
	e := newEcho()
	handleFunc(e, http.MethodGet, "/only-get", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	handleFunc(e, http.MethodGet, "/bad", func(*echo.Context) error { return echo.NewHTTPError(http.StatusBadRequest, "bad thing") })
	handleFunc(e, http.MethodGet, "/boom", func(*echo.Context) error { return errors.New("db down") })
	handleFunc(e, http.MethodPost, "/up", func(c *echo.Context) error {
		if _, err := io.Copy(io.Discard, c.Request().Body); err != nil {
			return err
		}
		return c.NoContent(http.StatusOK)
	})

	cases := []struct {
		name   string
		method string
		target string
		body   string
		code   int
		want   string
	}{
		{"404 no route", http.MethodGet, "/nope", "", 404, `{"status":{"code":"404","message":"Not Found"}}` + "\n"},
		{"405 wrong method", http.MethodPost, "/only-get", "", 405, `{"status":{"code":"405","message":"Method Not Allowed"}}` + "\n"},
		{"400 HTTPError", http.MethodGet, "/bad", "", 400, `{"status":{"code":"400","message":"bad thing"}}` + "\n"},
		{"500 plain error", http.MethodGet, "/boom", "", 500, `{"status":{"code":"5001","message":"internal server error"}}` + "\n"},
		{"413 over limit", http.MethodPost, "/up", strings.Repeat("x", 2_000_001), 413, `{"status":{"code":"413","message":"Request Entity Too Large"}}` + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveOnce(e, tc.method, tc.target, strings.NewReader(tc.body), nil)
			if rec.Code != tc.code {
				t.Errorf("status = %d, want %d", rec.Code, tc.code)
			}
			if got := rec.Body.String(); got != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
		})
	}
}

// 回應已經送出（committed）後 handler 又回錯誤，錯誤處理器不能再寫：狀態碼與 body 維持第一次送出的.
func TestCharacterizeCommittedResponseIsNotRewritten(t *testing.T) {
	e := newEcho()
	handleFunc(e, http.MethodGet, "/late", func(c *echo.Context) error {
		if err := c.String(http.StatusAccepted, "first"); err != nil {
			return err
		}
		return errors.New("late failure")
	})
	rec := serveOnce(e, http.MethodGet, "/late", http.NoBody, nil)
	if rec.Code != http.StatusAccepted || rec.Body.String() != "first" {
		t.Errorf("committed response changed: status %d body %q", rec.Code, rec.Body.String())
	}
}

// [F34-74] 全站 body 上限是 2,000,000 bytes（v4 的 "2M"）：剛好通過，多一個 byte 就 413 且 body 與 v4 相同.
func TestBodyLimitIsTwoMillionBytes(t *testing.T) {
	e := newEcho()
	handleFunc(e, http.MethodPost, "/up", func(c *echo.Context) error {
		n, err := io.Copy(io.Discard, c.Request().Body)
		if err != nil {
			return err
		}
		if n != 2_000_000 {
			return c.String(http.StatusInternalServerError, "short read")
		}
		return c.NoContent(http.StatusOK)
	})
	ok := serveOnce(e, http.MethodPost, "/up", strings.NewReader(strings.Repeat("x", 2_000_000)), nil)
	if ok.Code != http.StatusOK {
		t.Errorf("2,000,000 bytes: status = %d, want 200 (body %q)", ok.Code, ok.Body.String())
	}
	over := serveOnce(e, http.MethodPost, "/up", strings.NewReader(strings.Repeat("x", 2_000_001)), nil)
	if over.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("2,000,001 bytes: status = %d, want 413", over.Code)
	}
	if want := `{"status":{"code":"413","message":"Request Entity Too Large"}}` + "\n"; over.Body.String() != want {
		t.Errorf("413 body = %q, want %q", over.Body.String(), want)
	}
}

// [F34-73] panic → 500、經 slog（遮蔽後）記錄含路由樣式與堆疊，連線字串裡的帳密不會出現在 log.
func TestCharacterizePanicIsRedactedThroughSlog(t *testing.T) {
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch v := a.Value.Any().(type) {
			case string:
				return slog.String(a.Key, redact.String(v))
			case error:
				return slog.String(a.Key, redact.Error(v))
			}
			return a
		},
	})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newEcho()
	handleFunc(e, http.MethodGet, "/boom/:id", func(*echo.Context) error {
		panic("dial mongodb://alice:s3cretpw@db.internal:27017/app failed")
	})
	rec := serveRecovering(t, e, http.MethodGet, "/boom/42")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	out := buf.String()
	if strings.Contains(out, "s3cretpw") {
		t.Errorf("panic log leaks the credential: %s", out)
	}
	recs := slogRecords(t, buf, "panic recovered")
	if len(recs) != 1 {
		t.Fatalf("panic records = %d: %s", len(recs), out)
	}
	if recs[0]["path"] != "/boom/:id" {
		t.Errorf("path = %v, want the route pattern /boom/:id", recs[0]["path"])
	}
	if stack, _ := recs[0]["stack"].(string); !strings.Contains(stack, "goroutine") {
		t.Errorf("record carries no stack: %v", recs[0]["stack"])
	}
}

// swapStdStreams 把 os.Stdout / os.Stderr 換成管道，回傳讀回內容的函式（先還原再讀）. 不可 t.Parallel.
// 呼叫前必須已經把 slog 導到測試緩衝（slog.SetDefault 連帶改標準 log 的輸出，避免標準 log 握住舊 stderr）.
func swapStdStreams(t *testing.T) (collect func() (stdout, stderr string)) {
	t.Helper()
	type pipe struct {
		r, w *os.File
		data chan string
	}
	mk := func() *pipe {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		p := &pipe{r: r, w: w, data: make(chan string, 1)}
		go func() {
			b, _ := io.ReadAll(r)
			p.data <- string(b)
		}()
		return p
	}
	outP, errP := mk(), mk()
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outP.w, errP.w
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		os.Stdout, os.Stderr = origOut, origErr
	}
	t.Cleanup(restore)
	return func() (string, string) {
		restore()
		if err := outP.w.Close(); err != nil {
			t.Errorf("close stdout pipe: %v", err)
		}
		if err := errP.w.Close(); err != nil {
			t.Errorf("close stderr pipe: %v", err)
		}
		return <-outP.data, <-errP.data
	}
}

// [F34-73] panic 路徑 stdout 與 stderr 都是 0 bytes（堆疊只走 slog）.
func TestRecoverWritesNothingToStdoutOrStderr(t *testing.T) {
	captureServerSlog(t)
	e := newEcho()
	handleFunc(e, http.MethodGet, "/boom", func(*echo.Context) error { panic("kaboom") })
	collect := swapStdStreams(t)
	rec := serveRecovering(t, e, http.MethodGet, "/boom")
	stdout, stderr := collect()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("panic path wrote to the console: stdout %d bytes %q, stderr %d bytes %q", len(stdout), stdout, len(stderr), stderr)
	}
}

// [F34-71] serve 啟動到可連線的期間 stdout 與 stderr 都是 0 bytes（取代 v5 不存在的 HidePort）.
func TestServeWritesNothingToStdoutOrStderr(t *testing.T) {
	captureServerSlog(t)
	e := newEcho()
	collect := swapStdStreams(t)
	srv, addr, stop := startRunning(t, e)
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("server not connectable: %v", err)
	}
	_ = conn.Close()
	close(stop)
	if err := shutdownRunning(context.Background(), srv, nil); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	stdout, stderr := collect()
	if stdout != "" || stderr != "" {
		t.Errorf("startup wrote to the console: stdout %d bytes %q, stderr %d bytes %q", len(stdout), stdout, len(stderr), stderr)
	}
}

// [F34-71] serve 實際跑起來的那個 http.Server 帶著平台逾時值.
func TestStartedServerHasPlatformTimeouts(t *testing.T) {
	captureServerSlog(t)
	e := newEcho()
	srv, _, _ := startRunning(t, e)
	for name, c := range map[string][2]time.Duration{
		"ReadHeaderTimeout": {srv.ReadHeaderTimeout, 10 * time.Second},
		"ReadTimeout":       {srv.ReadTimeout, 30 * time.Second},
		"WriteTimeout":      {srv.WriteTimeout, 30 * time.Second},
		"IdleTimeout":       {srv.IdleTimeout, 2 * time.Minute},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %v, want %v", name, c[0], c[1])
		}
	}
}

// [F34-71] 背景工作拿到的 ctx 期限 = 呼叫 shutdown 的時刻 + shutdownTimeout（±1 秒）.
func TestShutdownDeadlineIsShutdownTimeout(t *testing.T) {
	e := newEcho()
	got := make(chan time.Time, 1)
	missing := make(chan struct{}, 1)
	stopper := Stopper{Name: "probe", Stop: func(ctx context.Context) error {
		d, ok := ctx.Deadline()
		if !ok {
			missing <- struct{}{}
			return nil
		}
		got <- d
		return nil
	}}
	called := time.Now()
	if err := shutdownRunning(context.Background(), newHTTPServer(e, "127.0.0.1:0"), []Stopper{stopper}); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-got:
		if diff := d.Sub(called); diff < shutdownTimeout-time.Second || diff > shutdownTimeout+time.Second {
			t.Errorf("deadline is %v after the call, want %v ± 1s", diff, shutdownTimeout)
		}
	case <-missing:
		t.Fatal("the background work ctx has no deadline")
	default:
		t.Fatal("the stopper was not called")
	}
}

// [F34-71] 一個卡住不回的進行中請求：shutdown 在 shutdownTimeout ± 1 秒內回傳，錯誤是 context.DeadlineExceeded.
// 自計時：逾時走 t.Fatal，不靠測試逾時或卡死呈現紅.
func TestShutdownCutsOffStuckRequestAtShutdownTimeout(t *testing.T) {
	t.Parallel()
	e := newEcho()
	entered := make(chan struct{})
	release := make(chan struct{})
	handleFunc(e, http.MethodGet, "/stuck", func(c *echo.Context) error {
		close(entered)
		<-release
		return c.NoContent(http.StatusOK)
	})
	srv, addr, stop := startRunning(t, e)
	t.Cleanup(func() {
		close(release)
		_ = srv.Close()
	})

	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/stuck", http.NoBody)
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the stuck request never reached the handler")
	}
	close(stop)

	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- shutdownRunning(context.Background(), srv, nil) }()
	select {
	case err := <-done:
		elapsed := time.Since(started)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("shutdown error = %v, want context.DeadlineExceeded", err)
		}
		if elapsed < shutdownTimeout-time.Second || elapsed > shutdownTimeout+time.Second {
			t.Errorf("shutdown returned after %v, want %v ± 1s", elapsed, shutdownTimeout)
		}
	case <-time.After(shutdownTimeout + 3*time.Second):
		t.Fatal("shutdown did not return within shutdownTimeout + 3s: the HTTP drain has no deadline")
	}
}

// [F34-75] 限流：ExtractIPDirect（沒設代理）時 X-Forwarded-For 偽造無效，同一個連線 IP 共用額度.
func TestCharacterizeRateLimitDirectIgnoresForwardedFor(t *testing.T) {
	e, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	e.Use(perIPRateLimiter(0.001, 2))
	handleFunc(e, http.MethodGet, "/r", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	hit := func(remote, xff string) *httptest.ResponseRecorder {
		return serveOnce(e, http.MethodGet, "/r", http.NoBody, func(r *http.Request) {
			r.RemoteAddr = remote
			if xff != "" {
				r.Header.Set("X-Forwarded-For", xff)
			}
		})
	}
	for i, xff := range []string{"1.1.1.1", "2.2.2.2"} {
		if rec := hit("198.51.100.9:5000", xff); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
	}
	rec := hit("198.51.100.9:5000", "3.3.3.3")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a spoofed X-Forwarded-For must not buy a fresh quota: status = %d, want 429", rec.Code)
	}
	if want := `{"status":{"code":"2001","message":"Too many requests"}}` + "\n"; rec.Body.String() != want {
		t.Errorf("429 body = %q, want %q", rec.Body.String(), want)
	}
	if rec := hit("198.51.100.10:5000", ""); rec.Code != http.StatusOK {
		t.Errorf("a different connection IP has its own quota: status = %d, want 200", rec.Code)
	}
}

// [F34-75] 限流：設了 TRUSTED_PROXIES 時，代理送來的 X-Forwarded-For 才是限流身分（不同用戶各自額度）.
func TestCharacterizeRateLimitTrustedProxiesUsesForwardedFor(t *testing.T) {
	e, err := New("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	e.Use(perIPRateLimiter(0.001, 1))
	handleFunc(e, http.MethodGet, "/r", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	hit := func(remote, xff string) int {
		return serveOnce(e, http.MethodGet, "/r", http.NoBody, func(r *http.Request) {
			r.RemoteAddr = remote
			r.Header.Set("X-Forwarded-For", xff)
		}).Code
	}
	if got := hit("10.1.2.3:443", "203.0.113.7"); got != http.StatusOK {
		t.Fatalf("first client via proxy: status = %d, want 200", got)
	}
	if got := hit("10.1.2.3:443", "203.0.113.7"); got != http.StatusTooManyRequests {
		t.Errorf("same client via proxy again: status = %d, want 429", got)
	}
	if got := hit("10.1.2.3:443", "203.0.113.8"); got != http.StatusOK {
		t.Errorf("another client via the same proxy has its own quota: status = %d, want 200", got)
	}
	// 不是設定的代理 → header 無效，以連線 IP 計.
	if got := hit("198.51.100.9:5000", "203.0.113.9"); got != http.StatusOK {
		t.Fatalf("direct client: status = %d, want 200", got)
	}
	if got := hit("198.51.100.9:5000", "203.0.113.10"); got != http.StatusTooManyRequests {
		t.Errorf("a non-proxy client cannot change identity with X-Forwarded-For: status = %d, want 429", got)
	}
}

// /health 與 /ready 不記請求日誌（保活與探測不洗版），其他路由要記.
func TestCharacterizeHealthAndReadyAreNotRequestLogged(t *testing.T) {
	buf := captureServerSlog(t)
	e := newEcho()
	for _, p := range []string{"/health", "/ready", "/other"} {
		handleFunc(e, http.MethodGet, p, func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	}
	for _, p := range []string{"/health", "/ready", "/other"} {
		serveOnce(e, http.MethodGet, p, http.NoBody, nil)
	}
	recs := slogRecords(t, buf, "request")
	if len(recs) != 1 || recs[0]["route"] != "/other" {
		t.Errorf("request log records = %v, want exactly one for /other", recs)
	}
}

// [F34-76] HSTS：Render 代理（私網來源）加 X-Forwarded-Proto: https 時送出；明文直連不送.
func TestCharacterizeHSTSBehindPrivateProxy(t *testing.T) {
	e := newEcho()
	handleFunc(e, http.MethodGet, "/h", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	rec := serveOnce(e, http.MethodGet, "/h", http.NoBody, func(r *http.Request) {
		r.RemoteAddr = "10.0.0.1:1234"
		r.Header.Set("X-Forwarded-Proto", "https")
	})
	if got, want := rec.Header().Get("Strict-Transport-Security"), "max-age=31536000; includeSubdomains"; got != want {
		t.Errorf("Strict-Transport-Security = %q, want %q", got, want)
	}
	plain := serveOnce(e, http.MethodGet, "/h", http.NoBody, func(r *http.Request) { r.RemoteAddr = "198.51.100.9:5000" })
	if got := plain.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("plain http must not send HSTS, got %q", got)
	}
}

// [F34-78] 405 的 Allow 標頭（值與順序）與 v4 相同：預期值取自 6bced56（v4.16.3）連跑 5 次的固定輸出
// "OPTIONS, GET"（單一方法）與 "OPTIONS, GET, POST, PUT"（三個方法，註冊順序）.
func TestCharacterizeMethodNotAllowedAllowHeader(t *testing.T) {
	e := newEcho()
	handleFunc(e, http.MethodGet, "/one", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
		handleFunc(e, m, "/multi", func(c *echo.Context) error { return c.NoContent(http.StatusOK) })
	}
	for _, tc := range []struct{ path, want string }{
		{"/one", "OPTIONS, GET"},
		{"/multi", "OPTIONS, GET, POST, PUT"},
	} {
		rec := serveOnce(e, http.MethodDelete, tc.path, http.NoBody, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", tc.path, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != tc.want {
			t.Errorf("%s: Allow = %q, want %q", tc.path, got, tc.want)
		}
	}
}
