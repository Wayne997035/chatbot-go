package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/geocoding"
	"chatbot-go/internal/line"
	weatherstore "chatbot-go/internal/storage/database/weather"
	"chatbot-go/internal/weather"
	"context"
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
