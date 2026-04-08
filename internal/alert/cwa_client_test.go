package alert

import (
	"chatbot-go/internal/models"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchWeatherWarnings_ParseResponse(t *testing.T) {
	mockResp := models.CWAAlertResponse{
		Success: "true",
		Records: models.CWAAlertRecords{
			Location: []models.CWAAlertLocation{
				{
					LocationName: "台北市",
					HazardCondition: models.CWAHazardCond{
						Hazards: []models.CWAHazard{
							{Info: models.CWAHazardInfo{Phenomena: "大雨", Significance: "特報"}},
						},
					},
				},
			},
		},
	}

	body, err := json.Marshal(mockResp)
	if err != nil {
		t.Fatalf("marshal mock response: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 驗證 auth key 透過 header 傳遞
		if got := r.Header.Get("Authorization"); got != "test-key" {
			t.Errorf("Authorization header = %q, want %q", got, "test-key")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	result, err := fetchAndDecode[models.CWAAlertResponse](context.Background(), srv.URL, "test-key")
	if err != nil {
		t.Fatalf("fetchAndDecode error: %v", err)
	}

	if result.Success != "true" {
		t.Errorf("Success = %q, want %q", result.Success, "true")
	}
	if len(result.Records.Location) != 1 {
		t.Fatalf("expected 1 location, got %d", len(result.Records.Location))
	}
	loc := result.Records.Location[0]
	if loc.LocationName != "台北市" {
		t.Errorf("LocationName = %q, want %q", loc.LocationName, "台北市")
	}
	if len(loc.HazardCondition.Hazards) != 1 {
		t.Fatalf("expected 1 hazard, got %d", len(loc.HazardCondition.Hazards))
	}
	if loc.HazardCondition.Hazards[0].Info.Phenomena != "大雨" {
		t.Errorf("Phenomena = %q, want %q", loc.HazardCondition.Hazards[0].Info.Phenomena, "大雨")
	}
}

func TestFetchAndDecode_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := fetchAndDecode[models.CWAAlertResponse](context.Background(), srv.URL, "test-key")
	if err == nil {
		t.Error("expected error for non-200 response")
	}
}

func TestFetchEarthquakes_ParseResponse(t *testing.T) {
	mockResp := models.CWAEarthquakeResponse{
		Success: "true",
		Records: models.CWAEarthquakeRecords{
			Earthquake: []models.CWAEarthquake{
				{
					EarthquakeNo:  113001,
					ReportType:    "地震報告",
					ReportContent: "規模5.0，最大震度4級",
					EarthquakeInfo: models.CWAEarthquakeInfo{
						OriginTime: "2024-01-15 10:00:00",
						FocalDepth: 10.0,
						EpiCenter:  models.CWAEpiCenter{Location: "花蓮縣近海"},
						Magnitude:  models.CWAMagnitude{Value: 5.0},
					},
				},
			},
		},
	}

	body, err := json.Marshal(mockResp)
	if err != nil {
		t.Fatalf("marshal mock response: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	result, err := fetchAndDecode[models.CWAEarthquakeResponse](context.Background(), srv.URL, "test-key")
	if err != nil {
		t.Fatalf("fetchAndDecode error: %v", err)
	}

	if result.Success != "true" {
		t.Errorf("Success = %q, want %q", result.Success, "true")
	}
	if len(result.Records.Earthquake) != 1 {
		t.Fatalf("expected 1 earthquake, got %d", len(result.Records.Earthquake))
	}
	eq := result.Records.Earthquake[0]
	if eq.EarthquakeNo != 113001 {
		t.Errorf("EarthquakeNo = %d, want 113001", eq.EarthquakeNo)
	}
	if eq.EarthquakeInfo.Magnitude.Value != 5.0 {
		t.Errorf("Magnitude = %f, want 5.0", eq.EarthquakeInfo.Magnitude.Value)
	}
}
