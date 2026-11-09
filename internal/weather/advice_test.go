package weather

import (
	"chatbot-go/internal/ai"
	"context"
	"strings"
	"testing"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

type fakeCompleter struct {
	answers []string
	prompts []string
}

// Complete 模擬路由：依序把答案丟給 Accept，第一個通過的就是結果.
func (f *fakeCompleter) Complete(_ context.Context, req *ai.Request) ai.Result {
	f.prompts = append(f.prompts, req.Prompt)
	for _, a := range f.answers {
		if req.Accept == nil || req.Accept(a) {
			return ai.Result{Text: a, Provider: "fake", Model: "m"}
		}
	}
	return ai.Result{Errors: []string{"fake: all rejected"}}
}

func rainyForecast() *weatherstore.WeatherForecast {
	return &weatherstore.WeatherForecast{
		City: "台北市", District: "信義區", CreateTime: "2026-11-02T09:00",
		Elements: []weatherstore.ForecastElement{
			{ElementName: "PoP12h", Description: "12小時降雨機率", Value: "70"},
			{ElementName: "AT", Description: "體感溫度", Value: "14"},
		},
	}
}

func TestAdvisorUsesAITip(t *testing.T) {
	c := &fakeCompleter{answers: []string{`{"tip":"今天濕冷，帶傘並加件外套"}`}}
	got := NewAdvisor(c, nil).Tip(context.Background(), rainyForecast())
	if got != "今天濕冷，帶傘並加件外套" {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(c.prompts[0], "台北市信義區") || !strings.Contains(c.prompts[0], "70%") {
		t.Errorf("prompt missing forecast: %s", c.prompts[0])
	}
}

func TestAdvisorRejectsUnusableAnswersAndFallsBackToRules(t *testing.T) {
	long := `{"tip":"` + strings.Repeat("很長", 50) + `"}`
	c := &fakeCompleter{answers: []string{"這不是 JSON", `{"tip":""}`, long}}
	got := NewAdvisor(c, nil).Tip(context.Background(), rainyForecast())
	if got != "降雨機率偏高，出門記得帶傘；氣溫偏低，記得多加件外套" {
		t.Errorf("got %q", got)
	}
}

func TestAdvisorWithoutAIUsesRules(t *testing.T) {
	if got := NewAdvisor(nil, nil).Tip(context.Background(), rainyForecast()); !strings.Contains(got, "帶傘") {
		t.Errorf("got %q", got)
	}
	var nilAdvisor *Advisor
	if nilAdvisor.Tip(context.Background(), rainyForecast()) != "" {
		t.Error("nil advisor should return empty tip")
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
func adviceLocalCacheCases(t *testing.T, newAdvisor func(Completer) *Advisor) {
	t.Helper()
	ctx := context.Background()

	t.Run("same key hits cache", func(t *testing.T) {
		c := &fakeCompleter{answers: []string{`{"tip":"今天帶傘"}`}}
		a := newAdvisor(c)
		first, second := a.Tip(ctx, rainyForecast()), a.Tip(ctx, rainyForecast())
		if len(c.prompts) != 1 {
			t.Errorf("prompts = %d, want 1", len(c.prompts))
		}
		if first != "今天帶傘" || second != first {
			t.Errorf("first=%q second=%q", first, second)
		}
	})

	t.Run("different CreateTime asks AI again", func(t *testing.T) {
		c := &fakeCompleter{answers: []string{`{"tip":"今天帶傘"}`}}
		a := newAdvisor(c)
		f2 := rainyForecast()
		f2.CreateTime = "2026-11-02T21:00"
		a.Tip(ctx, rainyForecast())
		a.Tip(ctx, f2)
		if len(c.prompts) != 2 {
			t.Errorf("prompts = %d, want 2", len(c.prompts))
		}
	})

	t.Run("rule fallback is not cached", func(t *testing.T) {
		c := &fakeCompleter{answers: nil} // AI 全拒絕
		a := newAdvisor(c)
		for i := 0; i < 2; i++ {
			if got := a.Tip(ctx, rainyForecast()); !strings.Contains(got, "帶傘") {
				t.Fatalf("call %d: got %q, want rule tip", i, got)
			}
		}
		if len(c.prompts) != 2 {
			t.Errorf("prompts = %d, want 2 (RuleTip must not be cached)", len(c.prompts))
		}
	})
}

// [F24-42] Redis 為 nil 時天氣建議走本地快取.
func TestAdvisorLocalCacheWhenRedisNil(t *testing.T) {
	adviceLocalCacheCases(t, func(c Completer) *Advisor { return NewAdvisor(c, nil) })
}

// [F24-42] Redis 出錯時天氣建議走本地快取，且不得回空字串或 panic.
func TestAdvisorLocalCacheWhenRedisErrors(t *testing.T) {
	dead := deadRedis(t)
	adviceLocalCacheCases(t, func(c Completer) *Advisor { return NewAdvisor(c, dead) })
}
