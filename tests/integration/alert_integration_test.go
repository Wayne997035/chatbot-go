package integration

import (
	"chatbot-go/internal/alert"
	"chatbot-go/internal/models"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// mockNotifier 記錄所有推播呼叫，用於驗證.
type mockNotifier struct {
	mu       sync.Mutex
	messages []pushRecord
}

type pushRecord struct {
	UserID string
	Text   string
}

func (n *mockNotifier) Push(_ context.Context, userID, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.messages = append(n.messages, pushRecord{UserID: userID, Text: text})
	return nil
}

func (n *mockNotifier) getMessages() []pushRecord {
	n.mu.Lock()
	defer n.mu.Unlock()
	cp := make([]pushRecord, len(n.messages))
	copy(cp, n.messages)
	return cp
}

// TestAlertSubRepo_CRUD 測試訂閱 CRUD 完整流程.
func TestAlertSubRepo_CRUD(t *testing.T) {
	db := getTestDB(t)
	repo := alertsub.NewAlertSubRepository(db)
	ctx := context.Background()

	// Upsert earthquake_tsunami subscription
	sub := &alertsub.AlertSubscription{
		ID:        "U001_earthquake_tsunami",
		UserID:    "U001",
		AlertType: alertsub.TypeEarthquakeTsunami,
		Enabled:   true,
	}
	if err := repo.Upsert(ctx, sub); err != nil {
		t.Fatalf("upsert earthquake sub: %v", err)
	}

	// Upsert weather_warning subscription with regions
	sub2 := &alertsub.AlertSubscription{
		ID:        "U001_weather_warning",
		UserID:    "U001",
		AlertType: alertsub.TypeWeatherWarning,
		Regions:   []string{"台北市", "新北市"},
		Enabled:   true,
	}
	if err := repo.Upsert(ctx, sub2); err != nil {
		t.Fatalf("upsert weather sub: %v", err)
	}

	// FindByUserID — should return 2
	subs, err := repo.FindByUserID(ctx, "U001")
	if err != nil {
		t.Fatalf("find by userID: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("expected 2 subs, got %d", len(subs))
	}

	// FindEnabledByType — earthquake_tsunami
	eqSubs, err := repo.FindEnabledByType(ctx, alertsub.TypeEarthquakeTsunami)
	if err != nil {
		t.Fatalf("find enabled by type: %v", err)
	}
	if len(eqSubs) != 1 || eqSubs[0].UserID != "U001" {
		t.Errorf("expected 1 earthquake sub for U001, got %d", len(eqSubs))
	}

	// FindEnabledByTypeAndRegion — 台北市
	regionSubs, err := repo.FindEnabledByTypeAndRegion(ctx, alertsub.TypeWeatherWarning, "台北市")
	if err != nil {
		t.Fatalf("find by type and region: %v", err)
	}
	if len(regionSubs) != 1 || regionSubs[0].UserID != "U001" {
		t.Errorf("expected 1 weather sub for 台北市, got %d", len(regionSubs))
	}

	// FindEnabledByTypeAndRegion — 高雄市 (no subscriber)
	noSubs, err := repo.FindEnabledByTypeAndRegion(ctx, alertsub.TypeWeatherWarning, "高雄市")
	if err != nil {
		t.Fatalf("find by unsubscribed region: %v", err)
	}
	if len(noSubs) != 0 {
		t.Errorf("expected 0 subs for 高雄市, got %d", len(noSubs))
	}

	// Update — disable earthquake subscription
	sub.Enabled = false
	if err := repo.Upsert(ctx, sub); err != nil {
		t.Fatalf("upsert disable: %v", err)
	}
	eqSubs, _ = repo.FindEnabledByType(ctx, alertsub.TypeEarthquakeTsunami)
	if len(eqSubs) != 0 {
		t.Errorf("disabled sub should not appear in enabled query, got %d", len(eqSubs))
	}

	// Delete
	if err := repo.Delete(ctx, "U001_weather_warning"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	subs, _ = repo.FindByUserID(ctx, "U001")
	if len(subs) != 1 {
		t.Errorf("after delete: expected 1 sub, got %d", len(subs))
	}
}

// TestCheckerWeatherWarning_Integration 測試天氣特報完整流程：mock CWA → Checker → 驗證推播.
func TestCheckerWeatherWarning_Integration(t *testing.T) {
	db := getTestDB(t)
	repo := alertsub.NewAlertSubRepository(db)
	ctx := context.Background()

	// 建立訂閱：U001 訂閱台北市天氣特報
	_ = repo.Upsert(ctx, &alertsub.AlertSubscription{
		ID: "U001_weather_warning", UserID: "U001",
		AlertType: alertsub.TypeWeatherWarning, Regions: []string{"台北市"}, Enabled: true,
	})
	// U002 訂閱高雄市（不應收到台北市的警報）
	_ = repo.Upsert(ctx, &alertsub.AlertSubscription{
		ID: "U002_weather_warning", UserID: "U002",
		AlertType: alertsub.TypeWeatherWarning, Regions: []string{"高雄市"}, Enabled: true,
	})

	// Mock CWA APIs
	weatherResp := models.CWAAlertResponse{
		Success: "true",
		Records: models.CWAAlertRecords{
			Location: []models.CWAAlertLocation{
				{
					LocationName: "臺北市", // CWA 回傳「臺」，checker 會轉「台」
					HazardCondition: models.CWAHazardCond{
						Hazards: []models.CWAHazard{
							{Info: models.CWAHazardInfo{Phenomena: "大雨特報", Significance: "Warning"}},
						},
					},
				},
			},
		},
	}

	eqResp := models.CWAEarthquakeResponse{
		Success: "true",
		Records: models.CWAEarthquakeRecords{Earthquake: []models.CWAEarthquake{}},
	}

	tsunamiResp := models.CWATsunamiResponse{
		Success: "true",
		Records: models.CWATsunamiRecords{Tsunami: []models.CWATsunami{}},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/W-C0033-001":
			_ = json.NewEncoder(w).Encode(weatherResp)
		case "/E-A0015-001":
			_ = json.NewEncoder(w).Encode(eqResp)
		case "/E-A0014-001":
			_ = json.NewEncoder(w).Encode(tsunamiResp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	notifier := &mockNotifier{}
	cfg := &config.Config{
		Weather: config.WeatherConfig{CWABaseURL: server.URL, CWAAuthKey: "test-key"},
		Alert: config.AlertConfig{
			WeatherDataset:    "W-C0033-001",
			EarthquakeDataset: "E-A0015-001",
			TsunamiDataset:    "E-A0014-001",
		},
	}

	checker := alert.NewChecker(repo, notifier, nil, cfg)
	if err := checker.CheckAndNotify(ctx); err != nil {
		t.Fatalf("CheckAndNotify: %v", err)
	}

	msgs := notifier.getMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 push, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].UserID != "U001" {
		t.Errorf("push to wrong user: got %s, want U001", msgs[0].UserID)
	}
}

// TestCheckerEarthquake_Integration 測試地震速報完整流程.
func TestCheckerEarthquake_Integration(t *testing.T) {
	db := getTestDB(t)
	repo := alertsub.NewAlertSubRepository(db)
	ctx := context.Background()

	// 兩個使用者訂閱地震海嘯
	_ = repo.Upsert(ctx, &alertsub.AlertSubscription{
		ID: "U001_earthquake_tsunami", UserID: "U001",
		AlertType: alertsub.TypeEarthquakeTsunami, Enabled: true,
	})
	_ = repo.Upsert(ctx, &alertsub.AlertSubscription{
		ID: "U002_earthquake_tsunami", UserID: "U002",
		AlertType: alertsub.TypeEarthquakeTsunami, Enabled: true,
	})

	weatherResp := models.CWAAlertResponse{
		Success: "true",
		Records: models.CWAAlertRecords{Location: []models.CWAAlertLocation{}},
	}

	eqResp := models.CWAEarthquakeResponse{
		Success: "true",
		Records: models.CWAEarthquakeRecords{
			Earthquake: []models.CWAEarthquake{
				{
					EarthquakeNo:  113072,
					ReportContent: "花蓮縣近海發生規模5.2地震",
					EarthquakeInfo: models.CWAEarthquakeInfo{
						OriginTime: "2025-03-15 14:32:00",
						FocalDepth: 15.3,
						EpiCenter:  models.CWAEpiCenter{Location: "花蓮縣近海"},
						Magnitude:  models.CWAMagnitude{Value: 5.2},
					},
				},
			},
		},
	}

	tsunamiResp := models.CWATsunamiResponse{
		Success: "true",
		Records: models.CWATsunamiRecords{Tsunami: []models.CWATsunami{}},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/W-C0033-001":
			_ = json.NewEncoder(w).Encode(weatherResp)
		case "/E-A0015-001":
			_ = json.NewEncoder(w).Encode(eqResp)
		case "/E-A0014-001":
			_ = json.NewEncoder(w).Encode(tsunamiResp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	notifier := &mockNotifier{}
	cfg := &config.Config{
		Weather: config.WeatherConfig{CWABaseURL: server.URL, CWAAuthKey: "test-key"},
		Alert: config.AlertConfig{
			WeatherDataset:    "W-C0033-001",
			EarthquakeDataset: "E-A0015-001",
			TsunamiDataset:    "E-A0014-001",
		},
	}

	checker := alert.NewChecker(repo, notifier, nil, cfg)
	if err := checker.CheckAndNotify(ctx); err != nil {
		t.Fatalf("CheckAndNotify: %v", err)
	}

	// 2 users should each get 1 push
	msgs := notifier.getMessages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 pushes, got %d: %+v", len(msgs), msgs)
	}

	userIDs := map[string]bool{}
	for _, m := range msgs {
		userIDs[m.UserID] = true
	}
	if !userIDs["U001"] || !userIDs["U002"] {
		t.Errorf("expected pushes to U001 and U002, got %v", userIDs)
	}
}

// TestCheckerTsunamiGreenFiltered_Integration 測試海嘯綠色（解除）不推播.
func TestCheckerTsunamiGreenFiltered_Integration(t *testing.T) {
	db := getTestDB(t)
	repo := alertsub.NewAlertSubRepository(db)
	ctx := context.Background()

	_ = repo.Upsert(ctx, &alertsub.AlertSubscription{
		ID: "U001_earthquake_tsunami", UserID: "U001",
		AlertType: alertsub.TypeEarthquakeTsunami, Enabled: true,
	})

	weatherResp := models.CWAAlertResponse{
		Success: "true",
		Records: models.CWAAlertRecords{Location: []models.CWAAlertLocation{}},
	}
	eqResp := models.CWAEarthquakeResponse{
		Success: "true",
		Records: models.CWAEarthquakeRecords{Earthquake: []models.CWAEarthquake{}},
	}

	// 綠色 = 解除，不應推播
	tsunamiResp := models.CWATsunamiResponse{
		Success: "true",
		Records: models.CWATsunamiRecords{
			Tsunami: []models.CWATsunami{
				{
					TsunamiNo:     1,
					ReportType:    "海嘯解除報告",
					ReportContent: "海嘯警報解除",
					ReportNo:      "1",
					ReportColor:   "綠色",
					EarthquakeInfo: models.CWAEarthquakeInfo{
						EpiCenter: models.CWAEpiCenter{Location: "日本東北"},
						Magnitude: models.CWAMagnitude{Value: 7.0},
					},
				},
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/W-C0033-001":
			_ = json.NewEncoder(w).Encode(weatherResp)
		case "/E-A0015-001":
			_ = json.NewEncoder(w).Encode(eqResp)
		case "/E-A0014-001":
			_ = json.NewEncoder(w).Encode(tsunamiResp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	notifier := &mockNotifier{}
	cfg := &config.Config{
		Weather: config.WeatherConfig{CWABaseURL: server.URL, CWAAuthKey: "test-key"},
		Alert: config.AlertConfig{
			WeatherDataset:    "W-C0033-001",
			EarthquakeDataset: "E-A0015-001",
			TsunamiDataset:    "E-A0014-001",
		},
	}

	checker := alert.NewChecker(repo, notifier, nil, cfg)
	if err := checker.CheckAndNotify(ctx); err != nil {
		t.Fatalf("CheckAndNotify: %v", err)
	}

	msgs := notifier.getMessages()
	if len(msgs) != 0 {
		t.Errorf("green tsunami should not trigger push, got %d: %+v", len(msgs), msgs)
	}
}
