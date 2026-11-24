package weather

import (
	"bytes"
	"chatbot-go/internal/platform/cwa"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

// failingUpsertRepo 前 failN 筆 Upsert 回錯，其餘成功；計數 Upsert 次數.
type failingUpsertRepo struct {
	fakeWeatherRepo
	failN int
	calls int
}

func (r *failingUpsertRepo) Upsert(ctx context.Context, f *weatherstore.WeatherForecast) error {
	r.calls++
	if r.calls <= r.failN {
		return errors.New("injected upsert failure")
	}
	return r.fakeWeatherRepo.Upsert(ctx, f)
}

// [F37-08] 一個批次（一次 fetchAndProcess）恰好一行 INFO「forecast batch written」，total/ok/failed 正確，沒有逐筆 INFO.
func TestFetchAndProcessLogsOneBatchLine(t *testing.T) {
	const n = 5
	resp := testCWAResponse()
	base := resp.Records.Locations[0].Location[0]
	resp.Records.Locations[0].Location = nil
	for i := 0; i < n; i++ {
		loc := base
		loc.LocationName = "區" + string(rune('甲'+i))
		resp.Records.Locations[0].Location = append(resp.Records.Locations[0].Location, loc)
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	client := testForecastClient("https://weather.example.test/api", "k", httpClient)

	tests := []struct {
		name     string
		failN    int
		wantOK   float64
		wantFail float64
	}{
		{"all ok", 0, n, 0},
		{"two fail", 2, n - 2, 2},
		{"all fail", n, 0, n},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			repo := &failingUpsertRepo{failN: tc.failN}
			spec := cwa.CallSpec{Stream: cwa.StreamForecast, Dataset: "F-D0047-TEST"}
			if err := fetchAndProcess(context.Background(), client, spec, time.UTC, repo); err != nil {
				t.Fatalf("fetchAndProcess: %v", err)
			}
			if repo.calls != n {
				t.Fatalf("Upsert calls = %d, want %d", repo.calls, n)
			}

			var batch, infos int
			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				var m map[string]any
				if err := json.Unmarshal([]byte(line), &m); err != nil {
					t.Fatalf("bad log line %q: %v", line, err)
				}
				if m["level"] == "INFO" {
					infos++
				}
				if m["msg"] != "forecast batch written" {
					continue
				}
				batch++
				if m["level"] != "INFO" || m["total"] != float64(n) || m["ok"] != tc.wantOK || m["failed"] != tc.wantFail {
					t.Errorf("batch line = %v, want INFO total=%d ok=%v failed=%v", m, n, tc.wantOK, tc.wantFail)
				}
			}
			if batch != 1 {
				t.Errorf("batch lines = %d, want exactly 1", batch)
			}
			if infos != 1 {
				t.Errorf("INFO lines = %d, want 1 (no per-row INFO)", infos)
			}
		})
	}
}
