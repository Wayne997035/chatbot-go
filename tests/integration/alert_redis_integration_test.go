package integration

import (
	"chatbot-go/internal/alert"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type staticSubs struct{ subs []alertsub.AlertSubscription }

func (s *staticSubs) FindByUserID(context.Context, string) ([]alertsub.AlertSubscription, error) {
	return nil, nil
}

func (s *staticSubs) FindEnabledByType(context.Context, string) ([]alertsub.AlertSubscription, error) {
	return s.subs, nil
}

func (s *staticSubs) FindEnabledByTypeAndRegion(context.Context, string, string) ([]alertsub.AlertSubscription, error) {
	return nil, nil
}

func (s *staticSubs) Upsert(context.Context, *alertsub.AlertSubscription) error { return nil }
func (s *staticSubs) Delete(context.Context, string) error                      { return nil }
func (s *staticSubs) DeleteByUser(context.Context, string) error                { return nil }

type countingNotifier struct {
	mu       sync.Mutex
	attempts map[string]int
	ok       map[string]int
	failOnce map[string]bool
}

func (n *countingNotifier) Push(_ context.Context, userID, _ string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.attempts[userID]++
	if n.failOnce[userID] {
		n.failOnce[userID] = false
		return errors.New("LINE API returned 500")
	}
	n.ok[userID]++
	return nil
}

// 逐人推播紀錄存在 Redis：重啟（新的 Checker）也不會重送；失敗的人下一輪只重試他.
func TestAlertCheckerDeliversOncePerUserOnRedis(t *testing.T) {
	client := setupTestRedis(t)
	quakeTime := time.Now().In(time.FixedZone("CST", 8*3600)).Add(-10 * time.Minute).Format("2006-01-02 15:04:05")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/E-A0015-001"):
			_, _ = fmt.Fprintf(w, `{"success":"true","records":{"Earthquake":[{"EarthquakeNo":115099,"EarthquakeInfo":{"OriginTime":%q,`+
				`"Epicenter":{"Location":"花蓮"},"EarthquakeMagnitude":{"MagnitudeValue":6.0}}}]}}`, quakeTime)
		case strings.HasSuffix(r.URL.Path, "/W-C0033-001"):
			_, _ = w.Write([]byte(`{"success":"true","records":{"location":[]}}`))
		default:
			_, _ = w.Write([]byte(`{"success":"true","records":{"Tsunami":[]}}`))
		}
	}))
	t.Cleanup(srv.Close)
	cfg := &config.Config{
		Weather: config.WeatherConfig{CWABaseURL: srv.URL, CWAAuthKey: "k"},
		Alert:   config.AlertConfig{WeatherDataset: "W-C0033-001", EarthquakeDataset: "E-A0015-001", TsunamiDataset: "E-A0014-001"},
	}
	subs := &staticSubs{subs: []alertsub.AlertSubscription{
		{UserID: "U1", AlertType: alertsub.TypeEarthquakeTsunami, Enabled: true},
		{UserID: "U2", AlertType: alertsub.TypeEarthquakeTsunami, Enabled: true},
	}}
	n := &countingNotifier{attempts: map[string]int{}, ok: map[string]int{}, failOnce: map[string]bool{"U2": true}}
	ctx := context.Background()

	for range 3 {
		// 每一輪都用新的 Checker，模擬程式重啟：紀錄只存在 Redis 裡.
		if err := alert.NewChecker(subs, n, client, cfg).CheckAndNotify(ctx); err != nil {
			t.Fatal(err)
		}
	}

	if n.ok["U1"] != 1 || n.attempts["U1"] != 1 {
		t.Errorf("U1: ok=%d attempts=%d, want 1/1", n.ok["U1"], n.attempts["U1"])
	}
	if n.ok["U2"] != 1 || n.attempts["U2"] != 2 {
		t.Errorf("U2 failed once then got it: ok=%d attempts=%d, want 1/2", n.ok["U2"], n.attempts["U2"])
	}
	if ttl := client.TTL(ctx, "alert:sent:earthquake:115099:U1").Val(); ttl <= 0 || ttl > 7*24*time.Hour {
		t.Errorf("sent record ttl = %v", ttl)
	}
}

// cancelOnPush 第一次推播時取消 ctx（模擬推到一半收到關機訊號）.
type cancelOnPush struct {
	countingNotifier
	cancel context.CancelFunc
}

func (n *cancelOnPush) Push(ctx context.Context, userID, text string) error {
	if n.cancel != nil {
		n.cancel()
		n.cancel = nil
		return ctx.Err()
	}
	return n.countingNotifier.Push(ctx, userID, text)
}

// 推播到一半關機：已佔位卻沒推成的人要釋放，重啟後照樣收到；不能卡在 Redis 裡 7 天收不到.
func TestAlertInterruptedByShutdownIsDeliveredAfterRestart(t *testing.T) {
	client := setupTestRedis(t)
	quakeTime := time.Now().In(time.FixedZone("CST", 8*3600)).Add(-10 * time.Minute).Format("2006-01-02 15:04:05")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/E-A0015-001"):
			_, _ = fmt.Fprintf(w, `{"success":"true","records":{"Earthquake":[{"EarthquakeNo":115100,"EarthquakeInfo":{"OriginTime":%q,`+
				`"Epicenter":{"Location":"花蓮"},"EarthquakeMagnitude":{"MagnitudeValue":6.0}}}]}}`, quakeTime)
		case strings.HasSuffix(r.URL.Path, "/W-C0033-001"):
			_, _ = w.Write([]byte(`{"success":"true","records":{"location":[]}}`))
		default:
			_, _ = w.Write([]byte(`{"success":"true","records":{"Tsunami":[]}}`))
		}
	}))
	t.Cleanup(srv.Close)
	cfg := &config.Config{
		Weather: config.WeatherConfig{CWABaseURL: srv.URL, CWAAuthKey: "k"},
		Alert:   config.AlertConfig{WeatherDataset: "W-C0033-001", EarthquakeDataset: "E-A0015-001", TsunamiDataset: "E-A0014-001"},
	}
	subs := &staticSubs{subs: []alertsub.AlertSubscription{
		{UserID: "U1", AlertType: alertsub.TypeEarthquakeTsunami, Enabled: true},
		{UserID: "U2", AlertType: alertsub.TypeEarthquakeTsunami, Enabled: true},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	n := &cancelOnPush{countingNotifier: countingNotifier{attempts: map[string]int{}, ok: map[string]int{}, failOnce: map[string]bool{}}, cancel: cancel}

	_ = alert.NewChecker(subs, n, client, cfg).CheckAndNotify(ctx) // 推第一個人時關機
	if err := alert.NewChecker(subs, n, client, cfg).CheckAndNotify(context.Background()); err != nil {
		t.Fatal(err)
	}

	if n.ok["U1"] != 1 || n.ok["U2"] != 1 {
		t.Errorf("after the restart everyone gets it once: %v", n.ok)
	}
}
