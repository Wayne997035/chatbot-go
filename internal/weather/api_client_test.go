package weather

import (
	"chatbot-go/internal/models"
	"testing"
	"time"
)

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

func TestFindClosestTime_Empty(t *testing.T) {
	result := findClosestTime(nil, time.Now())
	if result != nil {
		t.Error("expected nil for empty input")
	}
}
