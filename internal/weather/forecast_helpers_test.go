package weather

import (
	"bytes"
	"chatbot-go/internal/models"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"sync"
	"testing"
	"time"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

// fcstBuf 可並行寫入的 buffer（slog 捕捉用）.
type fcstBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *fcstBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *fcstBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fcstCaptureLogs 把預設 slog 導到 buffer，測完還原.
func fcstCaptureLogs(t *testing.T) *fcstBuf {
	t.Helper()
	b := &fcstBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(b, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return b
}

// fcstWaitFor 輪詢直到條件成立（最久 3 秒）.
func fcstWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fcstServer 記錄每個資料集被請求幾次的氣象署假伺服器.
type fcstServer struct {
	*httptest.Server
	mu     sync.Mutex
	counts map[string]int
	total  int
}

// fcstNewServer h 收到資料集代號與該資料集第幾次被請求（1 起算）；nil = 回合法預報.
func fcstNewServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request, dataset string, nth int)) *fcstServer {
	t.Helper()
	s := &fcstServer{counts: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dataset := path.Base(r.URL.Path)
		s.mu.Lock()
		s.counts[dataset]++
		s.total++
		nth := s.counts[dataset]
		s.mu.Unlock()
		if h == nil {
			fcstWriteBody(w, "中正區")
			return
		}
		h(w, r, dataset, nth)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *fcstServer) Total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

func (s *fcstServer) Count(dataset string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[dataset]
}

func (s *fcstServer) Datasets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.counts))
	for k := range s.counts {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// fcstBody 一個縣市、一個鄉鎮、一個「平均溫度」的預報回應.
func fcstBody(district, temp string) []byte {
	resp := models.CWAResponse{
		Success: "true",
		Records: models.CWARecords{Locations: []models.CWALocations{{
			LocationsName: "臺北市",
			Location: []models.CWALocation{{
				LocationName: district,
				WeatherElement: []models.CWAWeatherElement{{
					ElementName: "平均溫度",
					Time: []models.CWATime{{
						StartTime:    time.Now().Format(time.RFC3339),
						ElementValue: []models.CWAElementValue{{Temperature: temp}},
					}},
				}},
			}},
		}}},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		panic(err)
	}
	return b
}

func fcstWriteBody(w http.ResponseWriter, district string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(fcstBody(district, "28"))
}

// fcstHangUp 不回應就直接切斷連線（連線被重置）.
func fcstHangUp(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
}

// fcstClient 對 baseURL 建預報客戶端：重試退避不真睡；cfg 控制額度設定（nil = 預設）.
func fcstClient(baseURL string, cfg *config.Config, attemptTimeout time.Duration) *cwa.Client {
	if cfg == nil {
		cfg = &config.Config{}
	}
	settings := cfg.CWASettings()
	if attemptTimeout == 0 {
		attemptTimeout = cwa.ForecastAttemptTimeout
	}
	return cwa.NewClient(cwa.ClientOptions{
		BaseURL: baseURL, AuthKey: "test-key", AttemptTimeout: attemptTimeout,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}, cwa.NewLedger(nil, &settings, nil, time.Now), &settings)
}

// fcstPlainRepo 只實作 WeatherRepository 四個方法的假 repo（沒有 LatestCreateTimes）.
type fcstPlainRepo struct {
	mu        sync.Mutex
	forecasts []*weatherstore.WeatherForecast
}

func (r *fcstPlainRepo) FindByDistrict(context.Context, string) ([]weatherstore.WeatherForecast, error) {
	return nil, nil
}

func (r *fcstPlainRepo) FindByDistrictAndCity(context.Context, string, string) (*weatherstore.WeatherForecast, error) {
	return nil, nil
}

func (r *fcstPlainRepo) FindAllDistricts(context.Context) ([]weatherstore.DistrictEntry, error) {
	return nil, nil
}

func (r *fcstPlainRepo) Upsert(_ context.Context, f *weatherstore.WeatherForecast) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *f
	r.forecasts = append(r.forecasts, &cp)
	return nil
}

func (r *fcstPlainRepo) Stored() []*weatherstore.WeatherForecast {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.forecasts)
}

// fcstLatestRepo 另外實作 latestCreateTimer.
type fcstLatestRepo struct {
	fcstPlainRepo
	latest map[string]string
	err    error
}

func (r *fcstLatestRepo) LatestCreateTimes(context.Context) (map[string]string, error) {
	return r.latest, r.err
}

// fcstWeekDatasets 獨立於 forecastDatasets() 的期望清單：003、007、…、087 加 089.
func fcstWeekDatasets() []string {
	var ids []string
	for n := 3; n <= 87; n += 4 {
		ids = append(ids, fmt.Sprintf("F-D0047-%03d", n))
	}
	return append(ids, "F-D0047-089")
}

// fcstSwapSleep 換掉 rate limit 睡眠接縫並計次.
func fcstSwapSleep(t *testing.T) *int {
	t.Helper()
	var n int
	old := forecastSleep
	forecastSleep = func(context.Context, time.Duration) error { n++; return nil }
	t.Cleanup(func() { forecastSleep = old })
	return &n
}
