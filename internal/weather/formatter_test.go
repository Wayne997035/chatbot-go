package weather

import (
	"strings"
	"testing"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

func TestFormatSingleForecast(t *testing.T) {
	forecast := &weatherstore.WeatherForecast{
		City:     "台北市",
		District: "信義區",
		Elements: []weatherstore.ForecastElement{
			{ElementName: "T", Description: "平均溫度", Value: "28"},
			{ElementName: "RH", Description: "平均相對濕度", Value: "65"},
			{ElementName: "Wx", Description: "天氣現象", Value: "多雲"},
			{ElementName: "AT", Description: "體感溫度", Value: "30"},
			{ElementName: "PoP12h", Description: "12小時降雨機率", Value: "20"},
		},
	}

	result := FormatSingleForecast(forecast)

	tests := []struct {
		name     string
		contains string
	}{
		{"header", "台北市 信義區"},
		{"temperature unit", "28℃"},
		{"humidity unit", "65%"},
		{"weather no unit", "多雲"},
		{"apparent temp unit", "30℃"},
		{"precipitation unit", "20%"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(result, tt.contains) {
				t.Errorf("result should contain %q, got:\n%s", tt.contains, result)
			}
		})
	}
}

func TestFormatForecasts_Multiple(t *testing.T) {
	forecasts := []weatherstore.WeatherForecast{
		{City: "台北市", District: "信義區", Elements: []weatherstore.ForecastElement{
			{ElementName: "T", Description: "平均溫度", Value: "28"},
		}},
		{City: "新北市", District: "信義區", Elements: []weatherstore.ForecastElement{
			{ElementName: "T", Description: "平均溫度", Value: "27"},
		}},
	}

	result := FormatForecasts(forecasts)

	if !strings.Contains(result, "台北市") {
		t.Error("should contain 台北市")
	}
	if !strings.Contains(result, "新北市") {
		t.Error("should contain 新北市")
	}
	if !strings.Contains(result, "---") {
		t.Error("should contain separator")
	}
}

func TestFormatSingleForecast_EmptyValue(t *testing.T) {
	forecast := &weatherstore.WeatherForecast{
		City:     "台北市",
		District: "信義區",
		Elements: []weatherstore.ForecastElement{
			{ElementName: "T", Description: "平均溫度", Value: "28"},
			{ElementName: "RH", Description: "平均相對濕度", Value: ""},
		},
	}

	result := FormatSingleForecast(forecast)

	if strings.Contains(result, "平均相對濕度") {
		t.Error("should not contain empty value element")
	}
}
