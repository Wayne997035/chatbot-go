package alert

import (
	"bytes"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// [F30-80] 警報每次抓取的總時間不超過 FetchBudget：即使重試等待變慢（Sleep 一直等到 ctx 結束），
// 也要在約 FetchBudget（110ms）內回錯，而不是拖到父 ctx 的 2 秒.
func TestFetchJSONBoundedByFetchBudget(t *testing.T) {
	srv, _ := fetchServer(t, 1000, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	client := optClient(cwa.ClientOptions{
		BaseURL:        srv.URL,
		AttemptTimeout: 50 * time.Millisecond,
		Backoff:        10 * time.Millisecond,
		Sleep: func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := FetchWeatherWarnings(parent, client, "W-C0033-001")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error when every attempt gets 503")
	}
	if elapsed >= 500*time.Millisecond {
		t.Errorf("fetch took %v, want < 500ms (FetchBudget is %v, parent ctx is 2s)", elapsed, client.FetchBudget())
	}
}

// zcAttrHandler 收集 log 的訊息與屬性（既有 captureHandler 不留屬性）：JSON handler 寫進受鎖的 buffer 再解析.
type zcAttrHandler struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (h *zcAttrHandler) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.buf.Write(p)
}

// jobs 回傳指定訊息的每一行 log 的 job 欄位.
func (h *zcAttrHandler) jobs(t *testing.T, msg string) []string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for line := range bytes.SplitSeq(h.buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Msg string `json:"msg"`
			Job string `json:"job"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if rec.Msg == msg {
			out = append(out, rec.Job)
		}
	}
	return out
}

func zcCaptureAttrs(t *testing.T) *zcAttrHandler {
	t.Helper()
	h := &zcAttrHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(h, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return h
}

// [F30-84] 排程設 off 時，每個停用的排程印一行停用 log（帶 job 名），啟用的不印.
func TestAlertJobDisabledIsLogged(t *testing.T) {
	tests := []struct {
		name string
		mod  func(a *config.AlertConfig)
		want []string
	}{
		{"warning only", func(a *config.AlertConfig) { a.WarningCron = config.CronOff }, []string{jobWarning}},
		{"all three", func(a *config.AlertConfig) {
			a.WarningCron, a.EarthquakeCron, a.TsunamiCron = config.CronOff, config.CronOff, config.CronOff
		}, []string{jobWarning, jobEarthquake, jobTsunami}},
		{"none", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := zcCaptureAttrs(t) // 先於 startedScheduler：cleanup 後進先出，先停排程再還原 slog.
			feed := newRouteFeed(t)
			startedScheduler(t, feedConfig(feed, tt.mod), taipeiLoc(t), &fakeSubs{})

			got := logs.jobs(t, "alert job disabled")
			if len(got) != len(tt.want) {
				t.Fatalf("disabled log lines = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("disabled log line %d job = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// [F30-86] 特報總表記在 warning stream、海嘯記在 tsunami stream.
func TestExemptFetchStreams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(noWarnings))
	}))
	t.Cleanup(srv.Close)

	settings := (&config.Config{}).CWASettings()
	loc := taipeiLoc(t)
	ledger := cwa.NewLedger(nil, &settings, loc, nil)
	client := cwa.NewClient(cwa.ClientOptions{BaseURL: srv.URL, AuthKey: "test-key", Backoff: time.Millisecond}, ledger, &settings)

	if _, err := FetchWeatherWarnings(context.Background(), client, "W-C0033-001"); err != nil {
		t.Fatalf("warnings: %v", err)
	}
	if _, err := FetchTsunamiWarnings(context.Background(), client, "E-A0014-001"); err != nil {
		t.Fatalf("tsunami: %v", err)
	}

	by := ledger.Usage(context.Background()).ByStream
	if by[cwa.StreamWarning] != 1 || by[cwa.StreamTsunami] != 1 {
		t.Errorf("ByStream = %v, want warning=1 tsunami=1", by)
	}
	if by[cwa.StreamEarthquake] != 0 || by[cwa.StreamForecast] != 0 {
		t.Errorf("ByStream = %v, want earthquake=0 forecast=0", by)
	}
}
