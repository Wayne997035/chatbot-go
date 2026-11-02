package webhook

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/weather"
	"context"
	"encoding/json"
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

func newWeatherHandler(answer string) *WebhookHandler {
	repo := &fakeWeatherRepo{forecast: &weatherstore.WeatherForecast{
		City: "台北市", District: "信義區",
		Elements: []weatherstore.ForecastElement{{ElementName: "PoP12h", Description: "12小時降雨機率", Value: "80"}},
	}}
	h := NewWebhookHandler(nil, weather.NewLookup(repo, nil, 60), nil, nil)
	return h.WithAdvisor(weather.NewAdvisor(tipCompleter{answer: answer}, nil))
}

func TestWeatherReplyIncludesAITipAndQuickReplies(t *testing.T) {
	got := lineServer(t, 200, "{}")
	newWeatherHandler(`{"tip":"午後雷陣雨，記得帶傘"}`).handleLocationMessage(context.Background(), "臺北市信義區市府路1號", "tok")

	raw, _ := json.Marshal(got.payload)
	for _, want := range []string{"台北市 信義區", "80%", "建議：午後雷陣雨，記得帶傘", `"type":"location"`, "開啟天氣特報"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("reply missing %q: %s", want, raw)
		}
	}
}

func TestWeatherReplyFallsBackToRuleTipWhenAIFails(t *testing.T) {
	got := lineServer(t, 200, "{}")
	newWeatherHandler("not json").handleLocationMessage(context.Background(), "台北市信義區市府路1號", "tok")
	raw, _ := json.Marshal(got.payload)
	if !strings.Contains(string(raw), "建議：降雨機率偏高，出門記得帶傘") {
		t.Errorf("missing rule tip: %s", raw)
	}
	for _, leak := range []string{"rejected", "not json"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("AI error leaked to user: %s", raw)
		}
	}
}
