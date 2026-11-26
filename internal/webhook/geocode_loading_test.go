package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/geocoding"
	"chatbot-go/internal/line"
	weatherstore "chatbot-go/internal/storage/database/weather"
	"chatbot-go/internal/weather"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// orderedLineAPI [F39-02] 依呼叫順序記錄 /loading 與 /reply 的假 LINE API；loadingDelay 讓 /loading 回應變慢.
type orderedLineAPI struct {
	client       *line.Client
	mu           sync.Mutex
	events       []string
	replyAt      time.Time
	loadingDelay time.Duration
}

func newOrderedLineAPI(t *testing.T, loadingDelay time.Duration) *orderedLineAPI {
	t.Helper()
	api := &orderedLineAPI{loadingDelay: loadingDelay}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // 讀完 body，server 才會偵測到用戶端斷線（r.Context 才會結束）
		switch r.URL.Path {
		case "/loading":
			api.record("loading", false)
			select {
			case <-time.After(api.loadingDelay):
			case <-r.Context().Done():
			}
		case "/reply":
			api.record("reply", true)
		}
	}))
	t.Cleanup(srv.Close)
	api.client = line.NewClient(&line.Config{
		Token: "t", ReplyURL: srv.URL + "/reply", LoadingURL: srv.URL + "/loading",
	}, nil)
	return api
}

func (a *orderedLineAPI) record(ev string, isReply bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
	if isReply {
		a.replyAt = time.Now()
	}
}

func (a *orderedLineAPI) snapshot() (events []string, replyAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.events...), a.replyAt
}

func (a *orderedLineAPI) count(ev string) int {
	events, _ := a.snapshot()
	n := 0
	for _, e := range events {
		if e == ev {
			n++
		}
	}
	return n
}

// delayGeocoder [F39-02] 延遲 delay 後回 results 的假地名服務，記錄收到的 ctx 剩餘時間（沒有 deadline 記 -1）.
type delayGeocoder struct {
	delay   time.Duration
	results []geocoding.GeocodeResult
	mu      sync.Mutex
	budgets []time.Duration
}

func (g *delayGeocoder) Geocode(ctx context.Context, _ string) ([]geocoding.GeocodeResult, error) {
	g.mu.Lock()
	if dl, ok := ctx.Deadline(); ok {
		g.budgets = append(g.budgets, time.Until(dl))
	} else {
		g.budgets = append(g.budgets, -1)
	}
	g.mu.Unlock()
	select {
	case <-time.After(g.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.results, nil
}

func (g *delayGeocoder) recorded() []time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]time.Duration(nil), g.budgets...)
}

// 兩筆結果：意圖路徑與單打路徑都出選項卡（不再查天氣）.
func twoCandidates() []geocoding.GeocodeResult {
	return []geocoding.GeocodeResult{
		{City: "台北市", District: "信義區", DisplayName: "辦公室, 信義區, 台北市", Area: true},
		{City: "台北市", District: "北投區", DisplayName: "辦公室, 北投區, 台北市", Area: true},
	}
}

func newLoadingHandler(t *testing.T, api *orderedLineAPI, geo geocoding.Geocoder) *WebhookHandler {
	t.Helper()
	repo := &namedWeatherRepo{byDistrict: map[string][]weatherstore.WeatherForecast{}}
	return NewWebhookHandlerWithOptions(api.client, nil, weather.NewLookup(repo, nil, 60),
		&fakeAlertSubs{subs: map[string][]string{}}, conversation.NewMemoryManager(), geo, repo)
}

// geocodePaths 單打與意圖兩條路徑；回傳 true 表示該路徑已出卡.
var geocodePaths = []struct {
	name string
	run  func(h *WebhookHandler) bool
}{
	{"bare", func(h *WebhookHandler) bool {
		return h.handleBarePlaceGeocode(context.Background(), "U1", "天母", "tok")
	}},
	{"intent", func(h *WebhookHandler) bool {
		return h.handleGeocodingFallback(context.Background(), "U1", "天母", "tok")
	}},
}

// [F39-01] 意圖路徑（「X天氣」）呼叫地名服務時的 ctx 剩餘時間 > 2 秒且 ≤ 10 秒.
func TestIntentGeocodeHasTenSecondBudget(t *testing.T) {
	geo := &delayGeocoder{results: twoCandidates()}
	h := newLoadingHandler(t, newOrderedLineAPI(t, 0), geo)
	if !h.handleGeocodingFallback(context.Background(), "U1", "天母", "tok") {
		t.Fatal("intent path should have replied with candidates")
	}
	b := geo.recorded()
	if len(b) != 1 || b[0] <= barePlaceGeocodeTimeout || b[0] > 10*time.Second {
		t.Errorf("geocode budgets = %v, want one remaining time in (%v, 10s]", b, barePlaceGeocodeTimeout)
	}
}

// [F39-02] 地名服務慢（500 毫秒）：「輸入中」動畫恰 1 次，且排在回覆之前.
func TestSlowGeocodeStartsLoadingBeforeReply(t *testing.T) {
	for _, p := range geocodePaths {
		t.Run(p.name, func(t *testing.T) {
			api := newOrderedLineAPI(t, 0)
			h := newLoadingHandler(t, api, &delayGeocoder{delay: 500 * time.Millisecond, results: twoCandidates()})
			if !p.run(h) {
				t.Fatal("path should have replied with candidates")
			}
			events, _ := api.snapshot()
			if len(events) != 2 || events[0] != "loading" || events[1] != "reply" {
				t.Errorf("LINE API order = %v, want [loading reply]", events)
			}
		})
	}
}

// [F39-02][F40-03] 地名服務立即回（快取命中）：不送動畫，只有回覆.
// 延遲注入 1 小時：計時器不可能先觸發，結果不再靠 300 毫秒真時鐘的競爭.
func TestFastGeocodeSkipsLoading(t *testing.T) {
	for _, p := range geocodePaths {
		t.Run(p.name, func(t *testing.T) {
			api := newOrderedLineAPI(t, 0)
			h := newLoadingHandler(t, api, &delayGeocoder{results: twoCandidates()})
			h.loadingDelay = time.Hour
			if !p.run(h) {
				t.Fatal("path should have replied with candidates")
			}
			if api.count("loading") != 0 || api.count("reply") != 1 {
				events, _ := api.snapshot()
				t.Errorf("LINE API events = %v, want only one reply", events)
			}
		})
	}
}

// [F39-02] 動畫請求變慢（3 秒）不能拖慢回覆：StartLoading 有自己的 1 秒期限，從進入 handler 到 /reply < 2 秒.
func TestSlowLoadingCallDoesNotDelayReply(t *testing.T) {
	for _, p := range geocodePaths {
		t.Run(p.name, func(t *testing.T) {
			api := newOrderedLineAPI(t, 3*time.Second)
			h := newLoadingHandler(t, api, &delayGeocoder{delay: 500 * time.Millisecond, results: twoCandidates()})
			start := time.Now()
			if !p.run(h) {
				t.Fatal("path should have replied with candidates")
			}
			_, replyAt := api.snapshot()
			if replyAt.IsZero() {
				t.Fatal("no reply recorded")
			}
			if d := replyAt.Sub(start); d >= 2*time.Second {
				t.Errorf("reply came %v after entering the handler, want < 2s", d)
			}
		})
	}
}

// [F39-02] 地名服務 panic：兩條路徑都回 false、程序不崩，且立刻返回（不等到預算結束）.
func TestGeocoderPanicDoesNotCrashHandler(t *testing.T) {
	for _, p := range geocodePaths {
		t.Run(p.name, func(t *testing.T) {
			api := newOrderedLineAPI(t, 0)
			h := newLoadingHandler(t, api, panicGeocoder{})
			start := time.Now()
			if p.run(h) {
				t.Error("a panicking geocoder must fall through (false)")
			}
			if d := time.Since(start); d >= time.Second {
				t.Errorf("returned after %v, want < 1s", d)
			}
		})
	}
}

// loadingAwareGeocoder [F40-03] 等到看見 loading 呼叫才放行（最多等 wait），沒看見就照樣放行.
type loadingAwareGeocoder struct {
	api     *orderedLineAPI
	wait    time.Duration
	results []geocoding.GeocodeResult
}

func (g *loadingAwareGeocoder) Geocode(ctx context.Context, _ string) ([]geocoding.GeocodeResult, error) {
	deadline := time.Now().Add(g.wait)
	for g.api.count("loading") < 1 && time.Now().Before(deadline) {
		select {
		case <-time.After(2 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.results, nil
}

// [F40-03] loadingDelay=1ns 時兩條路徑各送 1 次 loading：守住 geocodeWithLoading 真的用注入值.
// geocoder 看到 loading 才放行、最多等 200ms；200ms 小於常數 300ms，若計時器改回常數，loading 不會出現、此測試失敗.
func TestLoadingDelayInjectionIsUsed(t *testing.T) {
	for _, p := range geocodePaths {
		t.Run(p.name, func(t *testing.T) {
			api := newOrderedLineAPI(t, 0)
			h := newLoadingHandler(t, api, &loadingAwareGeocoder{api: api, wait: 200 * time.Millisecond, results: twoCandidates()})
			h.loadingDelay = time.Nanosecond
			if !p.run(h) {
				t.Fatal("path should have replied with candidates")
			}
			if got := api.count("loading"); got != 1 {
				events, _ := api.snapshot()
				t.Errorf("loading count = %d (events %v), want 1", got, events)
			}
		})
	}
}

// keyRecordingGeocoder [F40-02] 記錄收到的 ctx 帶的閘門鑰匙.
type keyRecordingGeocoder struct {
	mu   sync.Mutex
	keys []string
	oks  []bool
}

func (g *keyRecordingGeocoder) Geocode(ctx context.Context, _ string) ([]geocoding.GeocodeResult, error) {
	k, ok := geocoding.GateKeyFrom(ctx)
	g.mu.Lock()
	g.keys, g.oks = append(g.keys, k), append(g.oks, ok)
	g.mu.Unlock()
	return twoCandidates(), nil
}

// [F40-02] 單打與意圖兩條路徑呼叫地名服務時，ctx 都帶著該 LINE 帳號當閘門鑰匙.
func TestGeocodeWithLoadingPassesUserKey(t *testing.T) {
	for _, p := range geocodePaths {
		t.Run(p.name, func(t *testing.T) {
			geo := &keyRecordingGeocoder{}
			h := newLoadingHandler(t, newOrderedLineAPI(t, 0), geo)
			h.loadingDelay = time.Hour
			if !p.run(h) {
				t.Fatal("path should have replied with candidates")
			}
			geo.mu.Lock()
			defer geo.mu.Unlock()
			if len(geo.keys) != 1 || !geo.oks[0] || geo.keys[0] != "U1" {
				t.Errorf("gate keys = %v (ok %v), want exactly [U1]", geo.keys, geo.oks)
			}
		})
	}
}
