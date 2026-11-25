package weather

import (
	"context"
	"strings"
	"testing"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

func rainyForecast() *weatherstore.WeatherForecast {
	return &weatherstore.WeatherForecast{
		City: "台北市", District: "信義區", CreateTime: "2026-11-02T09:00",
		Elements: []weatherstore.ForecastElement{
			{ElementName: "PoP12h", Description: "12小時降雨機率", Value: "70"},
			{ElementName: "AT", Description: "體感溫度", Value: "14"},
		},
	}
}

// [F36-06] W1：Advisor.Tip 內容 = RuleTip，不叫 AI（Advisor 已沒有任何 AI 依賴）；nil 安全.
func TestAdvisorTipIsRuleOnly(t *testing.T) {
	want := "降雨機率偏高，出門記得帶傘；氣溫偏低，記得多加件外套"
	if got := NewAdvisor().Tip(context.Background(), rainyForecast()); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, rule := NewAdvisor().Tip(context.Background(), rainyForecast()), RuleTip(rainyForecast()); got != rule {
		t.Errorf("Tip %q must equal RuleTip %q", got, rule)
	}
	var nilAdvisor *Advisor
	if nilAdvisor.Tip(context.Background(), rainyForecast()) != "" {
		t.Error("nil advisor should return empty tip")
	}
	if NewAdvisor().Tip(context.Background(), nil) != "" {
		t.Error("nil forecast should return empty tip")
	}
}

func TestRuleTip(t *testing.T) {
	hot := &weatherstore.WeatherForecast{Elements: []weatherstore.ForecastElement{
		{ElementName: "PoP12h", Value: "10"}, {ElementName: "T", Value: "34"},
	}}
	if got := RuleTip(hot); got != "天氣炎熱，注意防曬與補充水分" {
		t.Errorf("got %q", got)
	}
	mild := &weatherstore.WeatherForecast{Elements: []weatherstore.ForecastElement{{ElementName: "T", Value: "25"}, {ElementName: "PoP6h", Value: " "}}}
	if got := RuleTip(mild); got != "" {
		t.Errorf("got %q", got)
	}
}

// adviceLocalCacheCases 是 Redis nil 與 Redis 出錯共用的本地快取斷言.
// [F36-06] 快取已隨 AI 一起拔掉，這裡改斷言：每次都重算規則、不留狀態（不同 CreateTime、重複呼叫結果相同）.
func adviceLocalCacheCases(t *testing.T, a *Advisor) {
	t.Helper()
	ctx := context.Background()
	first, second := a.Tip(ctx, rainyForecast()), a.Tip(ctx, rainyForecast())
	f2 := rainyForecast()
	f2.CreateTime = "2026-11-02T21:00"
	if first == "" || first != second || a.Tip(ctx, f2) != first {
		t.Errorf("Tip must be a pure function of the forecast: first=%q second=%q", first, second)
	}
	changed := rainyForecast()
	changed.Elements[0].Value = "10" // 降雨機率掉到門檻下；有快取的話會還是舊句
	if got := a.Tip(ctx, changed); strings.Contains(got, "帶傘") {
		t.Errorf("a changed forecast with the same key must not reuse an old tip: %q", got)
	}
}

func TestAdvisorHasNoCache(t *testing.T) {
	adviceLocalCacheCases(t, NewAdvisor())
}
