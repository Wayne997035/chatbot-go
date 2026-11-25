package webhook

import (
	"regexp"
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

// addWeatherWordHint [F38-10] 任何「請在地名後加天氣」的教法（加天氣、加上「天氣」…）.
var addWeatherWordHint = regexp.MustCompile(`加上?「?天氣`)

// [F38-04][F38-10] 說明與「查天氣」按鈕回覆都要說單打「天母」「日月潭」可直接查；查不到改打鄉鎮區，不再教「後面加天氣再傳一次」.
func TestHelpAndWeatherPromptsSayPlacesCanBeTypedDirectly(t *testing.T) {
	f := newIntentFixture(t, "")
	help := f.say(t, "說明")
	button := f.say(t, keywordWeather)
	for name, got := range map[string]string{"help": help, "button": button} {
		mustContain(t, got, "天母", "日月潭")
		if strings.Contains(got, "地標請加「天氣」") || strings.Contains(got, "再傳一次") || addWeatherWordHint.MatchString(got) {
			t.Errorf("%s must not tell users to add the weather word and resend: %s", name, got)
		}
	}
	for name, got := range map[string]string{"weatherPromptMessage": weatherPromptMessage, "helpMessage": helpMessage} {
		if strings.Contains(got, "再傳一次") || addWeatherWordHint.MatchString(got) {
			t.Errorf("%s must not tell users to add the weather word and resend: %s", name, got)
		}
	}
	mustContain(t, weatherPromptMessage, "輸入地名", "大安區", "所在鄉鎮區")
	mustContain(t, helpMessage, "大安區", "所在鄉鎮區")
	mustContain(t, helpMessage, "天母、日月潭", "日月潭天氣")
}
