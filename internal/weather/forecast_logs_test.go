package weather

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// fcstForbidden 錯誤字串與 log 都不得出現的字樣：氣象署主機名、IP、解析器位址、完整 URL 片段.
var fcstForbidden = []string{"opendata.cwa.gov.tw", "10.0.0.2", "127.0.0.1", "?format=", `Get "`, "no such host", "http://", "https://"}

// fcstDNSClient 每個請求都以「DNS 形狀」的錯誤失敗（含主機名與解析器位址）.
func fcstDNSClient() *cwa.Client {
	settings := (&config.Config{}).CWASettings()
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "opendata.cwa.gov.tw", Server: "10.0.0.2:53", IsNotFound: true}
	})}
	return cwa.NewClient(cwa.ClientOptions{
		BaseURL: "https://opendata.cwa.gov.tw/api/v1/rest/datastore", AuthKey: "k", HTTPClient: httpClient,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}, cwa.NewLedger(nil, &settings, nil, time.Now), &settings)
}

// fcstLine 取第一行含 msg 的 log.
func fcstLine(out, msg string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, msg) {
			return l
		}
	}
	return ""
}

func fcstAssertClassOnly(t *testing.T, what, text string) {
	t.Helper()
	if text == "" {
		t.Fatalf("%s: no such log line", what)
	}
	if !strings.Contains(text, "class=dns") {
		t.Errorf("%s: want class=dns in %q", what, text)
	}
	for _, bad := range fcstForbidden {
		if strings.Contains(text, bad) {
			t.Errorf("%s: %q must not contain %q", what, text, bad)
		}
	}
}

// F30-54：預報端四個失敗 log 點各自只留分類（每處獨立斷言，不共用）.
func TestFetchFailureLogsAreRedacted(t *testing.T) {
	t.Run("fetch weather data", func(t *testing.T) {
		logs := fcstCaptureLogs(t)
		_, _ = FetchAndStore(context.Background(), fcstDNSClient(), 0, &fcstPlainRepo{})
		fcstAssertClassOnly(t, "fetch weather data", fcstLine(logs.String(), "fetch weather data"))
	})

	t.Run("returned all-failed error", func(t *testing.T) {
		_, err := FetchAndStore(context.Background(), fcstDNSClient(), 0, &fcstPlainRepo{})
		if err == nil {
			t.Fatal("want error")
		}
		for _, bad := range fcstForbidden {
			if strings.Contains(err.Error(), bad) {
				t.Errorf("error %q must not contain %q", err, bad)
			}
		}
		if !strings.Contains(err.Error(), "all 23 weather datasets failed") || !strings.Contains(err.Error(), "dns (2 attempts)") {
			t.Errorf("error = %q", err)
		}
	})

	t.Run("weather sync failed", func(t *testing.T) {
		logs := fcstCaptureLogs(t)
		s, err := NewScheduler(&fcstPlainRepo{}, &config.Config{}, fcstDNSClient(), fcstTaipei(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Stop(context.Background()) })
		s.syncWeather(context.Background())
		fcstAssertClassOnly(t, "weather sync failed", fcstLine(logs.String(), "weather sync failed"))
	})

	t.Run("manual weather sync failed", func(t *testing.T) {
		logs := fcstCaptureLogs(t)
		h := NewWeatherHandler(&fcstPlainRepo{}, &config.Config{}).WithCWAClient(fcstDNSClient())
		t.Cleanup(func() { _ = h.Stop(context.Background()) })
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/openDataUpdate", http.NoBody)
		if err := h.TriggerSync(echo.New().NewContext(req, httptest.NewRecorder())); err != nil {
			t.Fatal(err)
		}
		fcstWaitFor(t, "manual failure log", func() bool { return fcstLine(logs.String(), "manual weather sync failed") != "" })
		fcstAssertClassOnly(t, "manual weather sync failed", fcstLine(logs.String(), "manual weather sync failed"))
	})
}

// F30-54：回傳的錯誤是型別化的 *cwa.Error，字串不含伺服器位址（httptest 位址與 DNS 形狀各一）.
func TestFetchErrorsCarryNoServerAddress(t *testing.T) {
	tests := []struct {
		name      string
		client    func(t *testing.T) *cwa.Client
		wantClass string
	}{
		{
			name: "connection closed by httptest server",
			client: func(t *testing.T) *cwa.Client {
				srv := fcstNewServer(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { fcstHangUp(w) })
				return fcstClient(srv.URL, nil, 0)
			},
		},
		{name: "dns shaped error", client: func(*testing.T) *cwa.Client { return fcstDNSClient() }, wantClass: "dns"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FetchAndStore(context.Background(), tt.client(t), 0, &fcstPlainRepo{})
			if err == nil {
				t.Fatal("want error")
			}
			for _, bad := range fcstForbidden {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("error %q must not contain %q", err, bad)
				}
			}
			var ce *cwa.Error
			if !errors.As(err, &ce) {
				t.Fatalf("error %v does not carry a *cwa.Error", err)
			}
			if ce.Attempts != 2 || (tt.wantClass != "" && ce.Class != tt.wantClass) {
				t.Errorf("class = %q attempts = %d, want class %q and 2 attempts", ce.Class, ce.Attempts, tt.wantClass)
			}
		})
	}
}
