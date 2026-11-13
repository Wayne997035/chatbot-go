package cwa

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

var addrLeaks = []string{"127.0.0.1", "http://", "?format=", "opendata.cwa.gov.tw", "10.0.0.2", "a.example", "SECRET", `Get "`}

// F30-32：規格九列；各案例經 log 輸出後不含位址.
func TestClassifyErrors(t *testing.T) {
	refused := &net.OpError{
		Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1},
		Err: syscall.ECONNREFUSED,
	}
	reset := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	netTests := []struct {
		name string
		err  error
		want string
	}{
		{"dns shape", &url.Error{Op: "Get", URL: "https://opendata.cwa.gov.tw/x?format=JSON", Err: &net.DNSError{
			Err: "no such host", Name: "opendata.cwa.gov.tw", Server: "10.0.0.2:53", IsNotFound: true,
		}}, "dns"},
		{"x509 shape", errors.New("x509: certificate is valid for a.example, not opendata.cwa.gov.tw"), "tls"},
		{"x509 HostnameError type", x509.HostnameError{
			Certificate: &x509.Certificate{DNSNames: []string{"a.example"}}, Host: "opendata.cwa.gov.tw",
		}, "tls"},
		{"tls handshake timeout", errors.New("net/http: TLS handshake timeout"), "tls"},
		{"connection reset", reset, "conn_reset"},
		{"reset by message", errors.New("read tcp 10.0.0.2:5->10.0.0.9:443: read: connection reset by peer"), "conn_reset"},
		{"connection refused", refused, "network"},
		{"eof", io.EOF, "network"},
		{"deadline", context.DeadlineExceeded, "timeout"},
	}
	for _, tc := range netTests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			got := Classify(tc.err)
			if got != tc.want {
				t.Fatalf("Classify = %q, want %q", got, tc.want)
			}
			wrapped := &Error{Class: got, Attempts: 2, Err: tc.err}
			t.Logf("sample: %s", wrapped.Error())
			slogErr(wrapped)
			for _, bad := range addrLeaks {
				if strings.Contains(logs.String(), bad) || strings.Contains(wrapped.Error(), bad) {
					t.Fatalf("leaks %q: %s", bad, logs)
				}
			}
		})
	}
	// HTTP 503、壞 JSON、回應過大：由 Client 直接指定分類.
	clientTests := []struct {
		name    string
		handler func(http.ResponseWriter)
		want    string
	}{
		{"http 503", func(w http.ResponseWriter) { w.WriteHeader(503) }, "status_503"},
		{"bad json", func(w http.ResponseWriter) { _, _ = w.Write([]byte("{")) }, "decode"},
		{"too large", func(w http.ResponseWriter) { _, _ = w.Write([]byte(strings.Repeat("x", 200))) }, "too_large"},
	}
	for _, tc := range clientTests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			ts := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { tc.handler(w) })
			c := newTestClient(ts, nil, testSettings(nil), func(o *ClientOptions) { o.MaxBody = 100 })
			_, err := getOK(c, warnSpec)
			if ClassOf(err) != tc.want {
				t.Fatalf("class = %q, want %q (%v)", ClassOf(err), tc.want, err)
			}
			slogErr(err)
			for _, bad := range addrLeaks {
				if strings.Contains(logs.String(), bad) {
					t.Fatalf("leaks %q: %s", bad, logs)
				}
			}
		})
	}
}

// F30-33（共用）：log 一個錯誤（模擬下游 log 點）.
func slogErr(err error) {
	logger := slogDefault()
	logger.Error("alert check failed", "class", ClassOf(err), "error", err)
}

var classVocabulary = regexp.MustCompile(`^(dns|tls|timeout|conn_reset|network|decode|too_large|status_\d{3})$`)

// F30-32：回傳錯誤字串只有分類與次數，不含位址；Unwrap 不斷鏈.
func TestFetchErrorsCarryNoServerAddress(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		captureLogs(t)
		ts := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { okJSON(w) })
		c := newTestClient(ts, nil, testSettings(nil), nil)
		ts.Close() // 之後連不上：dial tcp 127.0.0.1:port: connection refused.
		_, err := getOK(c, warnSpec)
		if err == nil {
			t.Fatal("want error")
		}
		if err.Error() != "cwa fetch failed: network (2 attempts)" {
			t.Fatalf("error = %q", err.Error())
		}
		for _, bad := range addrLeaks {
			if strings.Contains(err.Error(), bad) {
				t.Fatalf("error leaks %q: %v", bad, err)
			}
		}
		var ce *Error
		if !errors.As(err, &ce) || !classVocabulary.MatchString(ce.Class) {
			t.Fatalf("errors.As / vocabulary failed: %v", err)
		}
	})
	t.Run("deadline unwraps", func(t *testing.T) {
		captureLogs(t)
		ts := newTestServer(t, func(_ int, _ http.ResponseWriter, r *http.Request) { blockUntilDone(r) })
		c := newTestClient(ts, nil, testSettings(nil), func(o *ClientOptions) { o.AttemptTimeout = 5 * time.Second })
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		var out decoded
		err := c.GetJSON(ctx, warnSpec, "x", url.Values{"format": {"JSON"}}, &out)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Unwrap chain broken: %v", err)
		}
		for _, bad := range addrLeaks {
			if strings.Contains(err.Error(), bad) {
				t.Fatalf("error leaks %q: %v", bad, err)
			}
		}
	})
	t.Run("Error text never includes inner message", func(t *testing.T) {
		e := &Error{Class: "dns", Attempts: 1, Err: errors.New("lookup opendata.cwa.gov.tw on 10.0.0.2:53: no such host")}
		if e.Error() != "cwa fetch failed: dns (1 attempts)" || !strings.Contains(e.Err.Error(), "lookup") {
			t.Fatalf("Error() = %q", e.Error())
		}
	})
}

// F30-32：D2b 回傳形狀 ①~⑤ 與 ClassOf.
func TestClientErrorShapes(t *testing.T) {
	ctx := context.Background()
	t.Run("1 success returns nil", func(t *testing.T) {
		captureLogs(t)
		ts := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { okJSON(w) })
		if _, err := getOK(newTestClient(ts, nil, testSettings(nil), nil), warnSpec); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("2 first attempt denied returns bare ErrQuotaReserved", func(t *testing.T) {
		captureLogs(t)
		clk := newTestClock(t)
		l := newMemLedger(t, clk, testSettings(nil))
		defaultNearFull(l, clk)
		ts := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { okJSON(w) })
		c := newTestClient(ts, l, testSettings(nil), nil)
		_, err := getOK(c, CallSpec{Stream: StreamCold, Dataset: "W-C0033-004"})
		var ce *Error
		if errors.As(err, &ce) || !errors.Is(err, ErrQuotaReserved) || err.Error() != ErrQuotaReserved.Error() {
			t.Fatalf("err = %#v, want bare ErrQuotaReserved", err)
		}
		if ts.count() != 0 || ClassOf(err) != "quota_reserved" {
			t.Fatalf("requests=%d class=%q", ts.count(), ClassOf(err))
		}
	})
	t.Run("3 later attempt denied wraps first failure class", func(t *testing.T) {
		captureLogs(t)
		clk := newTestClock(t)
		l := newMemLedger(t, clk, testSettings(nil))
		defaultNearFull(l, clk)
		ts := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
		c := newTestClient(ts, l, testSettings(nil), nil)
		_, err := getOK(c, CallSpec{Stream: StreamEarthquake, Exempt: true, Dataset: "E-A0015-001"})
		var ce *Error
		if !errors.As(err, &ce) || ce.Class != "status_503" || ce.Attempts != 1 || !errors.Is(ce.Err, ErrQuotaReserved) {
			t.Fatalf("err = %#v", err)
		}
		if !errors.Is(err, ErrQuotaReserved) || ClassOf(err) != "status_503" || ts.count() != 1 {
			t.Fatalf("errors.Is=%v class=%q reqs=%d", errors.Is(err, ErrQuotaReserved), ClassOf(err), ts.count())
		}
	})
	t.Run("4 other failures are *Error with fixed vocabulary", func(t *testing.T) {
		captureLogs(t)
		ts := newTestServer(t, func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
		_, err := getOK(newTestClient(ts, nil, testSettings(nil), nil), warnSpec)
		var ce *Error
		if !errors.As(err, &ce) || ce.Class != "status_404" || !classVocabulary.MatchString(ce.Class) {
			t.Fatalf("err = %#v", err)
		}
	})
	t.Run("5 parent canceled / expired", func(t *testing.T) {
		captureLogs(t)
		ts := newTestServer(t, func(_ int, _ http.ResponseWriter, r *http.Request) { blockUntilDone(r) })
		c := newTestClient(ts, nil, testSettings(nil), func(o *ClientOptions) { o.AttemptTimeout = 5 * time.Second })
		dctx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
		defer cancel()
		var out decoded
		err := c.GetJSON(dctx, warnSpec, "x", nil, &out)
		if ClassOf(err) != "timeout" || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline: %#v", err)
		}
		cctx, cancel2 := context.WithCancel(ctx)
		time.AfterFunc(40*time.Millisecond, cancel2)
		err = c.GetJSON(cctx, warnSpec, "x", nil, &out)
		if ClassOf(err) != "network" || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %#v", err)
		}
	})
	t.Run("ClassOf", func(t *testing.T) {
		for _, tc := range []struct {
			err  error
			want string
		}{
			{&Error{Class: "dns"}, "dns"},
			{ErrQuotaReserved, "quota_reserved"},
			{errors.New("anything"), "network"},
			{nil, "network"},
		} {
			if got := ClassOf(tc.err); got != tc.want {
				t.Fatalf("ClassOf(%v) = %q, want %q", tc.err, got, tc.want)
			}
		}
	})
}

// F30-37.
func TestLoadTaipeiFailureReturnsError(t *testing.T) {
	boom := errors.New("tzdata missing")
	loc, err := LoadTaipei(func(string) (*time.Location, error) { return nil, boom })
	if err == nil || !errors.Is(err, boom) || loc != nil {
		t.Fatalf("failure must return (nil, error) and never a fallback zone: loc=%v err=%v", loc, err)
	}
	var asked string
	loc, err = LoadTaipei(func(name string) (*time.Location, error) { asked = name; return time.LoadLocation(name) })
	if err != nil || loc == nil || asked != "Asia/Taipei" || loc.String() != "Asia/Taipei" {
		t.Fatalf("loc=%v asked=%q err=%v", loc, asked, err)
	}
	if loc, err = LoadTaipei(nil); err != nil || loc.String() != "Asia/Taipei" {
		t.Fatalf("default loader: loc=%v err=%v", loc, err)
	}
	var noLoc *time.Location
	if loc, err = LoadTaipei(func(string) (*time.Location, error) { return noLoc, nil }); err == nil || loc != nil {
		t.Fatalf("nil location must be an error: loc=%v err=%v", loc, err)
	}
}
