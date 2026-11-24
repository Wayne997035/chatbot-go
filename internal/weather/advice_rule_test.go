package weather

import (
	"strings"
	"testing"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

func forecastOf(els ...weatherstore.ForecastElement) *weatherstore.WeatherForecast {
	return &weatherstore.WeatherForecast{Elements: els}
}

func el(name, value string) weatherstore.ForecastElement {
	return weatherstore.ForecastElement{ElementName: name, Value: value}
}

const comfortLine = "天氣舒適，適合外出走走"

// TestRuleTipComfortableWeather [F36-08] W3：三條規則都沒命中且資料都讀得到，整句全等舒適句.
func TestRuleTipComfortableWeather(t *testing.T) {
	cases := []struct {
		name string
		f    *weatherstore.WeatherForecast
	}{
		{"PoP12h=20 AT=24", forecastOf(el("PoP12h", "20"), el("AT", "24"))},
		{"PoP6h=20 T=25", forecastOf(el("PoP6h", "20"), el("T", "25"))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RuleTip(c.f); got != comfortLine {
				t.Errorf("got %q, want %q", got, comfortLine)
			}
		})
	}
}

// TestRuleTipNoComfortLineWhenAnyRuleHits [F36-08] W3 第二支：任一規則命中時不得出現舒適句.
func TestRuleTipNoComfortLineWhenAnyRuleHits(t *testing.T) {
	cases := []struct {
		name string
		f    *weatherstore.WeatherForecast
	}{
		{"PoP12h=70", forecastOf(el("PoP12h", "70"), el("AT", "24"))},
		{"T=34", forecastOf(el("PoP12h", "20"), el("T", "34"))},
		{"AT=14", forecastOf(el("PoP12h", "20"), el("AT", "14"))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RuleTip(c.f)
			if got == "" {
				t.Fatal("rule should have hit, got empty tip")
			}
			if strings.Contains(got, "天氣舒適") {
				t.Errorf("got %q, must not contain comfort line", got)
			}
		})
	}
}

// TestRuleTipMissingDataStaysEmpty [F36-08] W4：資料缺漏不補舒適句.
func TestRuleTipMissingDataStaysEmpty(t *testing.T) {
	cases := []struct {
		name string
		f    *weatherstore.WeatherForecast
	}{
		{"only T=25", forecastOf(el("T", "25"))},
		{"only PoP12h=20", forecastOf(el("PoP12h", "20"))},
		{"T=25 PoP6h blank", forecastOf(el("T", "25"), el("PoP6h", " "))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RuleTip(c.f); got != "" {
				t.Errorf("got %q, want empty", got)
			}
		})
	}
}
