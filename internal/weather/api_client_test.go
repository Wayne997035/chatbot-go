package weather

import (
	"bytes"
	"chatbot-go/internal/models"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/cwa"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

var errUnexpectedFakeWeatherRepoCall = errors.New("unexpected fake weather repo call")

func TestExtractValue(t *testing.T) {
	tests := []struct {
		name string
		abbr string
		evs  []models.CWAElementValue
		want string
	}{
		{
			name: "temperature",
			abbr: "T",
			evs:  []models.CWAElementValue{{Temperature: "28"}},
			want: "28",
		},
		{
			name: "humidity",
			abbr: "RH",
			evs:  []models.CWAElementValue{{RelativeHumidity: "65"}},
			want: "65",
		},
		{
			name: "wind direction",
			abbr: "WD",
			evs:  []models.CWAElementValue{{WindDirection: "東北風"}},
			want: "東北風",
		},
		{
			name: "precipitation",
			abbr: "PoP12h",
			evs:  []models.CWAElementValue{{ProbabilityOfPrecipitation: "30"}},
			want: "30",
		},
		{
			name: "weather",
			abbr: "Wx",
			evs:  []models.CWAElementValue{{Weather: "晴"}},
			want: "晴",
		},
		{
			name: "empty slice",
			abbr: "T",
			evs:  nil,
			want: "",
		},
		{
			name: "unknown abbreviation",
			abbr: "UNKNOWN",
			evs:  []models.CWAElementValue{{Temperature: "28"}},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractValue(tt.abbr, tt.evs)
			if got != tt.want {
				t.Errorf("extractValue(%q) = %q, want %q", tt.abbr, got, tt.want)
			}
		})
	}
}

func TestParseTimeSafe(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantZero bool
	}{
		{"valid RFC3339", "2024-01-15T12:00:00+08:00", false},
		{"valid alt format", "2024-01-15T12:00:00+08:00", false},
		{"empty string", "", true},
		{"invalid format", "not-a-time", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseTimeSafe(tt.input)
			if tt.wantZero && !result.IsZero() {
				t.Error("expected zero time")
			}
			if !tt.wantZero && result.IsZero() {
				t.Error("expected non-zero time")
			}
		})
	}
}

func TestFindClosestTime(t *testing.T) {
	now := time.Date(2024, 1, 15, 13, 0, 0, 0, time.FixedZone("CST", 8*3600))

	times := []models.CWATime{
		{StartTime: "2024-01-15T06:00:00+08:00"},
		{StartTime: "2024-01-15T12:00:00+08:00"},
		{StartTime: "2024-01-15T18:00:00+08:00"},
	}

	closest := findClosestTime(times, now)
	if closest == nil {
		t.Fatal("expected non-nil")
	}

	if closest.StartTime != "2024-01-15T12:00:00+08:00" {
		t.Errorf("got %s, want 12:00", closest.StartTime)
	}
}

// 下午 1 點要顯示「06:00–18:00」這段，不能因為 18:00 開始的那段比較近就顯示今晚的預報.
func TestFindClosestTimePrefersThePeriodContainingNow(t *testing.T) {
	now := time.Date(2024, 1, 15, 13, 0, 0, 0, time.FixedZone("CST", 8*3600))
	times := []models.CWATime{
		{StartTime: "2024-01-15T06:00:00+08:00", EndTime: "2024-01-15T18:00:00+08:00"},
		{StartTime: "2024-01-15T18:00:00+08:00", EndTime: "2024-01-16T06:00:00+08:00"},
	}
	if got := findClosestTime(times, now); got == nil || got.StartTime != "2024-01-15T06:00:00+08:00" {
		t.Errorf("got %+v, want the 06:00–18:00 period", got)
	}
	late := time.Date(2024, 1, 15, 23, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if got := findClosestTime(times, late); got == nil || got.StartTime != "2024-01-15T18:00:00+08:00" {
		t.Errorf("at 23:00 got %+v, want the 18:00–06:00 period", got)
	}
}

func TestUVIndexIsReadFromItsOwnField(t *testing.T) {
	elements := extractElements([]models.CWAWeatherElement{{
		ElementName: "紫外線指數",
		Time: []models.CWATime{{
			StartTime: "2024-01-15T06:00:00+08:00", EndTime: "2024-01-15T18:00:00+08:00",
			ElementValue: []models.CWAElementValue{{UVIndex: "7", ComfortIndex: "20"}},
		}},
	}}, time.Date(2024, 1, 15, 13, 0, 0, 0, time.FixedZone("CST", 8*3600)))
	if len(elements) != 1 || elements[0].ElementName != "UVI" || elements[0].Value != "7" {
		t.Errorf("elements = %+v", elements)
	}
}

func TestFindClosestTime_Empty(t *testing.T) {
	result := findClosestTime(nil, time.Now())
	if result != nil {
		t.Error("expected nil for empty input")
	}
}

func TestFetchAndStoreSendsAuthKeyInHeader(t *testing.T) {
	const authKey = "test-cwa-auth-key"

	repo := &fakeWeatherRepo{}
	responseBody, err := json.Marshal(testCWAResponse())
	if err != nil {
		t.Fatalf("marshal test response: %v", err)
	}

	requestCount := 0
	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requestCount++

			if got := r.Header.Get("Authorization"); got != authKey {
				t.Errorf("Authorization header = %q, want %q", got, authKey)
			}
			if got := r.URL.Query().Get("Authorization"); got != "" {
				t.Errorf("Authorization query parameter = %q, want empty", got)
			}
			if got := r.URL.Query().Get("format"); got != "JSON" {
				t.Errorf("format query parameter = %q, want JSON", got)
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(responseBody)),
				Header:     make(http.Header),
				Request:    r,
			}, nil
		}),
	}
	client := testForecastClient("https://weather.example.test/api", authKey, httpClient)

	if _, err := FetchAndStore(context.Background(), client, 0, repo); err != nil {
		t.Fatalf("FetchAndStore() error = %v", err)
	}

	// 45 → 23：不再抓 4k+1 的 3 天資料集（F30-50）.
	if requestCount != 23 {
		t.Fatalf("request count = %d, want 23", requestCount)
	}
	if len(repo.forecasts) != 23 {
		t.Fatalf("stored forecast count = %d, want 23", len(repo.forecasts))
	}
}

type fakeWeatherRepo struct {
	forecasts []*weatherstore.WeatherForecast
}

func (r *fakeWeatherRepo) FindByDistrict(context.Context, string) ([]weatherstore.WeatherForecast, error) {
	return nil, errUnexpectedFakeWeatherRepoCall
}

func (r *fakeWeatherRepo) FindByDistrictAndCity(context.Context, string, string) (*weatherstore.WeatherForecast, error) {
	return nil, errUnexpectedFakeWeatherRepoCall
}

func (r *fakeWeatherRepo) FindAllDistricts(context.Context) ([]weatherstore.DistrictEntry, error) {
	return nil, errUnexpectedFakeWeatherRepoCall
}

func (r *fakeWeatherRepo) Upsert(_ context.Context, forecast *weatherstore.WeatherForecast) error {
	r.forecasts = append(r.forecasts, forecast)
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func testCWAResponse() models.CWAResponse {
	now := time.Now().Format(time.RFC3339)

	return models.CWAResponse{
		Success: "true",
		Records: models.CWARecords{
			Locations: []models.CWALocations{
				{
					LocationsName: "臺北市",
					Location: []models.CWALocation{
						{
							LocationName: "中正區",
							WeatherElement: []models.CWAWeatherElement{
								{
									ElementName: "平均溫度",
									Time: []models.CWATime{
										{
											StartTime: now,
											ElementValue: []models.CWAElementValue{
												{Temperature: "28"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// 授權碼錯誤（全部 401）時要回傳錯誤；以前會回傳成功、log 寫「同步完成」.
func TestFetchAndStoreFailsWhenEveryDatasetFails(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header), Request: r}, nil
	})}

	_, err := FetchAndStore(context.Background(), testForecastClient("https://cwa.example", "bad-key", httpClient), 0, &fakeWeatherRepo{})
	if err == nil || !strings.Contains(err.Error(), "all 23 weather datasets failed") {
		t.Errorf("err = %v", err)
	}
}

// testForecastClient 以假的 http client 建預報用客戶端（記憶體記帳、預設設定）.
func testForecastClient(baseURL, authKey string, httpClient *http.Client) *cwa.Client {
	settings := (&config.Config{}).CWASettings()
	return cwa.NewClient(cwa.ClientOptions{
		BaseURL: baseURL, AuthKey: authKey, HTTPClient: httpClient, AttemptTimeout: cwa.ForecastAttemptTimeout,
	}, cwa.NewLedger(nil, &settings, nil, time.Now), &settings)
}
