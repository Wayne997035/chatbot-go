package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/geocoding"
	"chatbot-go/internal/weather"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// stubGeocoder 回固定結果的假 geocoder.
type stubGeocoder struct{ results []geocoding.GeocodeResult }

func (s *stubGeocoder) Geocode(context.Context, string) ([]geocoding.GeocodeResult, error) {
	return s.results, nil
}

// replyAltText 取最後一則回覆的 altText.
func replyAltText(t *testing.T, raw string) string {
	t.Helper()
	var payload struct {
		Messages []struct {
			AltText string `json:"altText"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil || len(payload.Messages) == 0 {
		t.Fatalf("parse reply %q: %v", raw, err)
	}
	return payload.Messages[0].AltText
}

// TestGeocodeSingleBusStopAsksToConfirm [F241-14] 唯一結果只來自公車站時出一顆按鈕的確認卡片，不直接回天氣；
// 一般單一結果照舊直接回天氣；多個結果維持原本文字.
func TestGeocodeSingleBusStopAsksToConfirm(t *testing.T) {
	const forecastMarker = "降雨機率"

	newFixture := func(t *testing.T, results []geocoding.GeocodeResult) (*WebhookHandler, *lineAPI, *placeWeatherRepo) {
		t.Helper()
		api := newLineAPI(t, nil)
		repo := &placeWeatherRepo{}
		h := NewWebhookHandlerWithOptions(api.client, nil, weather.NewLookup(repo, nil, 60), nil,
			conversation.NewMemoryManager(), &stubGeocoder{results: results}, repo)
		return h, api, repo
	}

	t.Run("confirm: single bus-stop-only result shows a one-button card", func(t *testing.T) {
		h, api, repo := newFixture(t, []geocoding.GeocodeResult{{City: "金門縣", District: "烈嶼鄉", NeedsConfirm: true}})
		h.handleTextMessage(context.Background(), "U1", "福壽山", "tok")
		raw := api.lastReply(t)

		if got := replyAltText(t, raw); got != "找到一個符合的地點，請確認" {
			t.Errorf("altText = %q, want %q", got, "找到一個符合的地點，請確認")
		}
		if n := strings.Count(raw, "找到一個符合的地點"); n != 2 {
			t.Errorf("%q appears %d times, want 2 (altText and card title): %s", "找到一個符合的地點", n, raw)
		}
		if strings.Contains(raw, "找到多個符合的地點") {
			t.Errorf("reply must not use the multi-result wording: %s", raw)
		}
		if n := countFooterButtons(t, raw); n != 1 {
			t.Errorf("buttons = %d, want 1", n)
		}
		if !strings.Contains(raw, `"text":"金門縣烈嶼鄉"`) {
			t.Errorf("button should send 金門縣烈嶼鄉: %s", raw)
		}
		if strings.Contains(raw, forecastMarker) {
			t.Errorf("reply must not contain a forecast: %s", raw)
		}
		if len(repo.calls) != 0 {
			t.Errorf("weather queried before the user confirmed: %v", repo.calls)
		}

		state, err := h.convManager.Get(context.Background(), "U1")
		if err != nil || state == nil || state.Type != conversation.TypeLocationClarify || len(state.Candidates) != 1 {
			t.Fatalf("state = %+v err=%v, want one location-clarify candidate", state, err)
		}

		h.handleTextMessage(context.Background(), "U1", "金門縣烈嶼鄉", "tok2")
		if len(repo.calls) != 1 || repo.calls[0] != [2]string{"烈嶼鄉", "金門縣"} {
			t.Errorf("after confirming, weather queries = %v, want [(烈嶼鄉, 金門縣)]", repo.calls)
		}
		if !strings.Contains(api.lastReply(t), forecastMarker) {
			t.Errorf("confirming should reply with the forecast: %s", api.lastReply(t))
		}
	})

	t.Run("direct: same result without NeedsConfirm replies with the forecast", func(t *testing.T) {
		h, api, repo := newFixture(t, []geocoding.GeocodeResult{{City: "金門縣", District: "烈嶼鄉"}})
		h.handleTextMessage(context.Background(), "U1", "福壽山", "tok")
		raw := api.lastReply(t)

		if !strings.Contains(raw, forecastMarker) {
			t.Errorf("reply should contain the forecast: %s", raw)
		}
		if strings.Contains(raw, "altText") || strings.Contains(raw, "找到一個符合的地點") {
			t.Errorf("reply must not be a candidate card: %s", raw)
		}
		if len(repo.calls) != 1 || repo.calls[0] != [2]string{"烈嶼鄉", "金門縣"} {
			t.Errorf("weather queries = %v, want [(烈嶼鄉, 金門縣)]", repo.calls)
		}
	})

	t.Run("many: two results keep the multi-result wording and two buttons", func(t *testing.T) {
		h, api, _ := newFixture(t, []geocoding.GeocodeResult{
			{City: "台中市", District: "和平區", NeedsConfirm: true},
			{City: "台東縣", District: "延平鄉"},
		})
		h.handleTextMessage(context.Background(), "U1", "武陵", "tok")
		raw := api.lastReply(t)

		if got := replyAltText(t, raw); got != "找到多個符合的地點，請選擇" {
			t.Errorf("altText = %q, want %q", got, "找到多個符合的地點，請選擇")
		}
		if strings.Contains(raw, "找到一個符合的地點") {
			t.Errorf("reply must not use the single-result wording: %s", raw)
		}
		if n := countFooterButtons(t, raw); n != 2 {
			t.Errorf("buttons = %d, want 2", n)
		}
	})
}
