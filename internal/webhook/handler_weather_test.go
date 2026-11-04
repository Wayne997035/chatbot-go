package webhook

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/line"
	"chatbot-go/internal/models"
	"chatbot-go/internal/weather"
	"context"
	"strings"
	"testing"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

type fakeWeatherRepo struct {
	forecast *weatherstore.WeatherForecast
}

func (f *fakeWeatherRepo) FindByDistrict(context.Context, string) ([]weatherstore.WeatherForecast, error) {
	return []weatherstore.WeatherForecast{*f.forecast}, nil
}

func (f *fakeWeatherRepo) FindByDistrictAndCity(context.Context, string, string) (*weatherstore.WeatherForecast, error) {
	return f.forecast, nil
}

func (f *fakeWeatherRepo) FindAllDistricts(context.Context) ([]weatherstore.DistrictEntry, error) {
	return nil, nil
}

func (f *fakeWeatherRepo) Upsert(context.Context, *weatherstore.WeatherForecast) error { return nil }

type tipCompleter struct{ answer string }

func (c tipCompleter) Complete(_ context.Context, req *ai.Request) ai.Result {
	if req.Accept != nil && !req.Accept(c.answer) {
		return ai.Result{Errors: []string{"rejected"}}
	}
	return ai.Result{Text: c.answer}
}

func newWeatherHandler(lc *line.Client, answer string) *WebhookHandler {
	repo := &fakeWeatherRepo{forecast: &weatherstore.WeatherForecast{
		City: "台北市", District: "信義區",
		Elements: []weatherstore.ForecastElement{{ElementName: "PoP12h", Description: "12小時降雨機率", Value: "80"}},
	}}
	h := NewWebhookHandler(lc, nil, weather.NewLookup(repo, nil, 60), nil, nil)
	return h.WithAdvisor(weather.NewAdvisor(tipCompleter{answer: answer}, nil))
}

func TestWeatherReplyIncludesAITipAndQuickReplies(t *testing.T) {
	api := newLineAPI(t, nil)
	newWeatherHandler(api.client, `{"tip":"午後雷陣雨，記得帶傘"}`).handleLocationMessage(context.Background(), "U1", "臺北市信義區市府路1號", "tok")

	raw := api.lastReply(t)
	for _, want := range []string{"台北市 信義區", "80%", "建議：午後雷陣雨，記得帶傘", `"type":"location"`, "開啟天氣特報"} {
		if !strings.Contains(raw, want) {
			t.Errorf("reply missing %q: %s", want, raw)
		}
	}
}

func TestWeatherReplyFallsBackToRuleTipWhenAIFails(t *testing.T) {
	api := newLineAPI(t, nil)
	newWeatherHandler(api.client, "not json").handleLocationMessage(context.Background(), "U1", "台北市信義區市府路1號", "tok")
	raw := api.lastReply(t)
	if !strings.Contains(raw, "建議：降雨機率偏高，出門記得帶傘") {
		t.Errorf("missing rule tip: %s", raw)
	}
	for _, leak := range []string{"rejected", "not json"} {
		if strings.Contains(raw, leak) {
			t.Errorf("AI error leaked to user: %s", raw)
		}
	}
}

// TestCandidateFlexLabelFitsLineLimit [F24-03].
func TestCandidateFlexLabelFitsLineLimit(t *testing.T) {
	candidates := []conversation.CandidateLocation{
		{DisplayName: strings.Repeat("臺", 60)},
		{DisplayName: strings.Repeat("😀", 30)},
		{DisplayName: "士林區"},
	}
	wants := []struct{ label, text string }{
		{strings.Repeat("臺", 39) + "…", "1"},
		{strings.Repeat("😀", 19) + "…", "2"},
		{"士林區", "3"},
	}

	bubble := buildCandidateFlexMessage(candidates)
	if len(bubble.Footer.Contents) != len(wants) {
		t.Fatalf("buttons = %d, want %d", len(bubble.Footer.Contents), len(wants))
	}
	for i, w := range wants {
		btn, ok := bubble.Footer.Contents[i].(models.ButtonComponent)
		if !ok {
			t.Fatalf("button %d type = %T", i, bubble.Footer.Contents[i])
		}
		if btn.Action.Label != w.label {
			t.Errorf("button %d label = %q, want %q", i, btn.Action.Label, w.label)
		}
		if btn.Action.Text != w.text {
			t.Errorf("button %d text = %q, want %q", i, btn.Action.Text, w.text)
		}
	}
}
