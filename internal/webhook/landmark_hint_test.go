package webhook

import (
	"slices"
	"strings"
	"testing"
)

// [F36-04] L2：「地標＋天氣」含提示字，照舊走意圖 AI 抽地名再查.
func TestLandmarkWithWeatherHintStillUsesIntentAI(t *testing.T) {
	f, geo := newGeoFixture(t, answerJSON("weather_query", "日月潭", "high"), yuchi)
	mustContain(t, f.say(t, "日月潭天氣"), "降雨機率")
	if !slices.Equal(geo.calls, []string{"日月潭"}) || f.ai.count() != 1 {
		t.Errorf("geocoder calls = %v intent = %d, want [日月潭] and 1", geo.calls, f.ai.count())
	}
}

// [F36-03] G3：沒有提示字的短句（G1 六句）走 handleTextMessage，意圖 Completer 呼叫 0 次.
func TestShortMessagesSkipIntentAI(t *testing.T) {
	f := newIntentFixture(t, answerJSON("none", "", "high"))
	for _, text := range []string{"日月潭", "天母", "平安", "早安", "一二三四五六七八", " 天母 "} {
		f.say(t, text)
	}
	if f.ai.count() != 0 {
		t.Errorf("intent completer calls = %d, want 0 for short messages without hint words", f.ai.count())
	}
}

// [F38-04] 說明與「查天氣」按鈕回覆都要說單打「天母」「日月潭」可直接查、查不到才加「天氣」；不再寫「地標請加「天氣」」.
func TestHelpAndWeatherPromptsSayPlacesCanBeTypedDirectly(t *testing.T) {
	f := newIntentFixture(t, "")
	help := f.say(t, "說明")
	button := f.say(t, keywordWeather)
	for name, got := range map[string]string{"help": help, "button": button} {
		mustContain(t, got, "天母", "日月潭", "加「天氣」")
		if strings.Contains(got, "地標請加「天氣」") {
			t.Errorf("%s must not tell users a landmark needs the weather word first: %s", name, got)
		}
	}
	mustContain(t, helpMessage, "天母、日月潭", "日月潭天氣")
}
