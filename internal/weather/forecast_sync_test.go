package weather

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// F30-50：清單 23 份、與共用層的 planned 推算一致、無重複、格式正確.
func TestForecastDatasetsMatchCount(t *testing.T) {
	got := forecastDatasets()
	if len(got) != cwa.ForecastDatasetCount {
		t.Fatalf("len = %d, want cwa.ForecastDatasetCount = %d", len(got), cwa.ForecastDatasetCount)
	}
	re := regexp.MustCompile(`^F-D0047-\d{3}$`)
	seen := map[string]bool{}
	for _, id := range got {
		if !re.MatchString(id) {
			t.Errorf("dataset %q does not match F-D0047-\\d{3}", id)
		}
		if seen[id] {
			t.Errorf("duplicate dataset %q", id)
		}
		seen[id] = true
	}
}

// F30-50：只請求 1 週那 22 份加 089，各 1 次；任何 4k+1、091、093 都不得出現.
func TestFetchAndStoreRequestsOnlyForecastDatasets(t *testing.T) {
	srv := fcstNewServer(t, nil)
	repo := &fcstPlainRepo{}

	res, err := FetchAndStore(context.Background(), fcstClient(srv.URL, nil, 0), 0, repo)
	if err != nil {
		t.Fatalf("FetchAndStore: %v", err)
	}
	want := fcstWeekDatasets()
	if got := srv.Datasets(); !slices.Equal(got, want) {
		t.Fatalf("requested datasets = %v, want %v", got, want)
	}
	for _, id := range want {
		if n := srv.Count(id); n != 1 {
			t.Errorf("%s requested %d times, want 1", id, n)
		}
	}
	for _, banned := range []string{"F-D0047-001", "F-D0047-005", "F-D0047-085", "F-D0047-091", "F-D0047-093"} {
		if srv.Count(banned) != 0 {
			t.Errorf("%s must not be requested", banned)
		}
	}
	if srv.Total() != 23 || res != (SyncResult{Fetched: 23}) {
		t.Errorf("total = %d, result = %+v", srv.Total(), res)
	}
}

// F30-50：3 天資料集不被請求、也不會寫入（它們回不同的溫度，寫入就會被看見）.
func TestFetchAndStoreOnlyOneWeekDataWritten(t *testing.T) {
	week := map[string]bool{}
	for _, id := range fcstWeekDatasets() {
		week[id] = true
	}
	srv := fcstNewServer(t, func(w http.ResponseWriter, _ *http.Request, dataset string, _ int) {
		temp := "99" // 3 天資料集的溫度
		if week[dataset] {
			temp = "28"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fcstBody("中正區", temp))
	})
	repo := &fcstPlainRepo{}

	if _, err := FetchAndStore(context.Background(), fcstClient(srv.URL, nil, 0), 0, repo); err != nil {
		t.Fatalf("FetchAndStore: %v", err)
	}
	stored := repo.Stored()
	if len(stored) != 23 {
		t.Fatalf("stored = %d, want 23", len(stored))
	}
	for _, f := range stored {
		if len(f.Elements) != 1 || f.Elements[0].Value != "28" {
			t.Errorf("%s stored elements %+v: three-day data leaked in", f.Dataset, f.Elements)
		}
	}
}

// F30-50：每份第 1 次連線被重置、第 2 次成功 → 23 份全成功、各恰 2 次；永遠重置 → 各恰 2 次、全部失敗.
func TestFetchAndStoreRetriesTransientErrors(t *testing.T) {
	tests := []struct {
		name        string
		failAlways  bool
		wantStored  int
		wantErrText string
	}{
		{name: "reset once then ok", wantStored: 23},
		{name: "always reset", failAlways: true, wantErrText: "all 23 weather datasets failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fcstNewServer(t, func(w http.ResponseWriter, _ *http.Request, _ string, nth int) {
				if tt.failAlways || nth == 1 {
					fcstHangUp(w)
					return
				}
				fcstWriteBody(w, "中正區")
			})
			repo := &fcstPlainRepo{}
			_, err := FetchAndStore(context.Background(), fcstClient(srv.URL, nil, 0), 0, repo)
			if tt.wantErrText == "" && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tt.wantErrText != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErrText)) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErrText)
			}
			for _, id := range fcstWeekDatasets() {
				if n := srv.Count(id); n != 2 {
					t.Errorf("%s requested %d times, want exactly 2", id, n)
				}
			}
			if got := len(repo.Stored()); got != tt.wantStored {
				t.Errorf("stored = %d, want %d", got, tt.wantStored)
			}
		})
	}
}

// F30-50：401 / 403 / 404 / 429 / 200 但 JSON 壞 都不重試：每份恰 1 次、全部失敗.
func TestFetchAndStoreDoesNotRetryPermanentErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"401", http.StatusUnauthorized, "{}"},
		{"403", http.StatusForbidden, "{}"},
		{"404", http.StatusNotFound, "{}"},
		{"429", http.StatusTooManyRequests, "{}"},
		{"200 bad json", http.StatusOK, "not json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fcstNewServer(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			// 429 預設會讓共用層暫停低優先呼叫（見下一個測試）；這裡把清單換掉，單獨驗「4xx 不重試」.
			cfg := &config.Config{CWA: config.CWAConfig{ExhaustedStatusCodes: "418"}}
			res, err := FetchAndStore(context.Background(), fcstClient(srv.URL, cfg, 0), 0, &fcstPlainRepo{})
			if err == nil || !strings.Contains(err.Error(), "all 23 weather datasets failed") {
				t.Fatalf("err = %v", err)
			}
			if res != (SyncResult{Failed: 23}) {
				t.Errorf("result = %+v", res)
			}
			for _, id := range fcstWeekDatasets() {
				if n := srv.Count(id); n != 1 {
					t.Errorf("%s requested %d times, want exactly 1", id, n)
				}
			}
		})
	}
}

// F30-50：預設設定下 429 讓共用層暫停低優先呼叫：第 1 份失敗，其餘 22 份被略過（沒有請求），整體不算全部失敗.
func TestForecastDefaultPauseOn429SkipsTheRest(t *testing.T) {
	srv := fcstNewServer(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	res, err := FetchAndStore(context.Background(), fcstClient(srv.URL, nil, 0), 0, &fcstPlainRepo{})
	if err != nil {
		t.Fatalf("err = %v, want nil (skipped datasets are not failures)", err)
	}
	if res != (SyncResult{Failed: 1, Skipped: 22}) || srv.Total() != 1 {
		t.Errorf("result = %+v, requests = %d", res, srv.Total())
	}
}

// F30-50：每次嘗試各自的期限：第 1 次嘗試卡住超過期限被放棄、第 2 次立即成功.
func TestFetchAndStorePerAttemptTimeout(t *testing.T) {
	const slow = "F-D0047-003"
	srv := fcstNewServer(t, func(w http.ResponseWriter, r *http.Request, dataset string, nth int) {
		if dataset == slow && nth == 1 {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		fcstWriteBody(w, "中正區")
	})
	repo := &fcstPlainRepo{}
	start := time.Now()
	res, err := FetchAndStore(context.Background(), fcstClient(srv.URL, nil, 100*time.Millisecond), 0, repo)
	if err != nil {
		t.Fatalf("FetchAndStore: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("elapsed = %v: the slow first attempt was not abandoned at its own deadline", elapsed)
	}
	if res != (SyncResult{Fetched: 23}) || srv.Count(slow) != 2 || srv.Total() != 24 {
		t.Errorf("result = %+v, slow requests = %d, total = %d", res, srv.Count(slow), srv.Total())
	}
}

// F30-50：被額度擋下的資料集算略過、不算失敗、不睡 rateLimitMs.
func TestForecastSkippedByQuotaIsNotFailure(t *testing.T) {
	tests := []struct {
		name          string
		dailyLimit    int
		want          SyncResult
		wantRequests  int
		wantSleeps    int
		wantCompleted string // 空 = 不得印 completed
	}{
		{name: "all blocked", dailyLimit: 1, want: SyncResult{Skipped: 23}},
		{
			name: "last ten blocked", dailyLimit: 14, want: SyncResult{Fetched: 13, Skipped: 10},
			wantRequests: 13, wantSleeps: 13, wantCompleted: "fetched=13 skipped=10 failed=0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := fcstCaptureLogs(t)
			sleeps := fcstSwapSleep(t)
			srv := fcstNewServer(t, nil)
			cfg := &config.Config{CWA: config.CWAConfig{DailyLimit: tt.dailyLimit}}

			res, err := FetchAndStore(context.Background(), fcstClient(srv.URL, cfg, 0), 2000, &fcstPlainRepo{})
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if res != tt.want {
				t.Errorf("result = %+v, want %+v", res, tt.want)
			}
			if srv.Total() != tt.wantRequests || *sleeps != tt.wantSleeps {
				t.Errorf("requests = %d (want %d), sleeps = %d (want %d)", srv.Total(), tt.wantRequests, *sleeps, tt.wantSleeps)
			}
			out := logs.String()
			if strings.Contains(out, "all 23 weather datasets failed") {
				t.Errorf("skipped datasets must not read as failure: %s", out)
			}
			if tt.wantCompleted == "" {
				if !strings.Contains(out, "weather sync skipped by quota") || strings.Contains(out, "weather sync completed") {
					t.Errorf("all-blocked logs: %s", out)
				}
				return
			}
			if !strings.Contains(out, "weather sync completed") || !strings.Contains(out, tt.wantCompleted) {
				t.Errorf("want completed with %q, got: %s", tt.wantCompleted, out)
			}
		})
	}
}

// F30-50：第 1 次嘗試失敗、重試時才被額度擋下，是真的失敗（請求已發出），不能算略過.
func TestForecastRetryBlockedByQuotaIsFailureNotSkip(t *testing.T) {
	logs := fcstCaptureLogs(t)
	cfg := &config.Config{CWA: config.CWAConfig{DailyLimit: 2}} // cap = floor(1.9) = 1：第 1 次嘗試放行、重試被擋
	srv := fcstNewServer(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { fcstHangUp(w) })

	res, err := FetchAndStore(context.Background(), fcstClient(srv.URL, cfg, 0), 0, &fcstPlainRepo{})
	if err != nil {
		t.Fatalf("err = %v, want nil (the other 22 were skipped)", err)
	}
	if res != (SyncResult{Failed: 1, Skipped: 22}) || srv.Total() != 1 {
		t.Errorf("result = %+v, requests = %d", res, srv.Total())
	}
	if out := logs.String(); !strings.Contains(out, "fetch weather data") || strings.Contains(out, "127.0.0.1") {
		t.Errorf("the failed dataset must be logged by class only: %s", out)
	}
}

// F30-52：寫入的每一筆 Dataset = 來源資料集代號，且 JSON 回應不含 dataset.
func TestUpsertWritesDatasetField(t *testing.T) {
	srv := fcstNewServer(t, func(w http.ResponseWriter, _ *http.Request, dataset string, _ int) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fcstBody("區"+dataset, "28")) // 把來源資料集編進鄉鎮名
	})
	repo := &fcstPlainRepo{}
	if _, err := FetchAndStore(context.Background(), fcstClient(srv.URL, nil, 0), 0, repo); err != nil {
		t.Fatal(err)
	}
	stored := repo.Stored()
	if len(stored) != 23 {
		t.Fatalf("stored = %d", len(stored))
	}
	for _, f := range stored {
		if want := strings.TrimPrefix(f.District, "區"); f.Dataset != want {
			t.Errorf("Dataset = %q, want %q", f.Dataset, want)
		}
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "dataset") {
			t.Errorf("API JSON must not expose dataset: %s", b)
		}
	}
}

// F30-52：createTime 一律以 Asia/Taipei 格式化，與容器本地時區無關.
func TestForecastCreateTimeWrittenInTaipei(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = oldLocal })
	oldNow := forecastNow
	forecastNow = func() time.Time { return time.Date(2026, 11, 10, 22, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { forecastNow = oldNow })

	srv := fcstNewServer(t, nil)
	repo := &fcstPlainRepo{}
	if _, err := FetchAndStore(context.Background(), fcstClient(srv.URL, nil, 0), 0, repo); err != nil {
		t.Fatal(err)
	}
	stored := repo.Stored()
	if len(stored) == 0 {
		t.Fatal("nothing stored")
	}
	for _, f := range stored {
		if f.CreateTime != "2026-11-11 06:00:00" {
			t.Fatalf("createTime = %q, want 2026-11-11 06:00:00 (Taipei)", f.CreateTime)
		}
	}
}

// F30-52：時區載入失敗 → 不發任何請求、不寫入、回 taipei timezone unavailable（不退回 UTC）.
func TestForecastTaipeiUnavailableNoRequests(t *testing.T) {
	old := forecastLoadTaipei
	forecastLoadTaipei = func() (*time.Location, error) { return nil, errors.New("open zoneinfo at 10.0.0.9:53 failed") }
	t.Cleanup(func() { forecastLoadTaipei = old })

	srv := fcstNewServer(t, nil)
	repo := &fcstPlainRepo{}
	res, err := FetchAndStore(context.Background(), fcstClient(srv.URL, nil, 0), 0, repo)
	if err == nil || !strings.Contains(err.Error(), "taipei timezone unavailable") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "10.0.0.9") {
		t.Errorf("error leaks inner message: %v", err)
	}
	if srv.Total() != 0 || len(repo.Stored()) != 0 || res != (SyncResult{}) {
		t.Errorf("requests = %d, stored = %d, result = %+v", srv.Total(), len(repo.Stored()), res)
	}
}
