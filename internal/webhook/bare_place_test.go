package webhook

import (
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/geocoding"
	"chatbot-go/internal/intent"
	weatherstore "chatbot-go/internal/storage/database/weather"
	"chatbot-go/internal/weather"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// bareGeocoder [F38-02] 依查詢字回結果的假地名服務：記錄每一次呼叫的查詢字；err 非 nil 時一律回錯誤.
type bareGeocoder struct {
	byQuery map[string][]geocoding.GeocodeResult
	err     error
	calls   []string
	// budgets [F38-07] 每次呼叫收到的 ctx 剩多少時間；沒有 deadline 記 -1.
	budgets []time.Duration
}

func (g *bareGeocoder) Geocode(ctx context.Context, text string) ([]geocoding.GeocodeResult, error) {
	g.calls = append(g.calls, text)
	if dl, ok := ctx.Deadline(); ok {
		g.budgets = append(g.budgets, time.Until(dl))
	} else {
		g.budgets = append(g.budgets, -1)
	}
	if g.err != nil {
		return nil, g.err
	}
	return g.byQuery[text], nil
}

// 夾具照 Nominatim 真實回傳的 category：天母 place/suburb、陽明山 place/village + highway/bus_stop、日月潭 water/reservoir、幸福只有車站與站牌.
var (
	tianmuArea    = geocoding.GeocodeResult{City: "台北市", District: "士林區", DisplayName: "天母, 士林區, 台北市", Area: true}
	yangmingArea  = geocoding.GeocodeResult{City: "台北市", District: "北投區", DisplayName: "陽明山, 北投區, 台北市", Area: true}
	kinmenStop    = geocoding.GeocodeResult{City: "金門縣", District: "金湖鎮", DisplayName: "陽明山公車站, 金湖鎮, 金門縣", NeedsConfirm: true}
	riyuetanArea  = geocoding.GeocodeResult{City: "南投縣", District: "魚池鄉", DisplayName: "日月潭, 魚池鄉, 南投縣", Area: true}
	xingfuStation = geocoding.GeocodeResult{City: "台東縣", District: "關山鎮", DisplayName: "幸福車站, 關山鎮, 台東縣"}
)

func bareGeo() *bareGeocoder {
	return &bareGeocoder{byQuery: map[string][]geocoding.GeocodeResult{
		"天母":  {tianmuArea},
		"陽明山": {yangmingArea, kinmenStop},
		"日月潭": {riyuetanArea},
		"幸福":  {xingfuStation},
	}}
}

// newBareFixture 官方地名只有「大安區」、geocoder 用 geo；withAI=false 時問答與意圖 AI 都停用.
func newBareFixture(t *testing.T, geo *bareGeocoder, intentAnswer string, withAI bool) *intentFixture {
	t.Helper()
	f := newIntentFixture(t, intentAnswer)
	repo := &namedWeatherRepo{byDistrict: map[string][]weatherstore.WeatherForecast{"大安區": {forecastOf("台北市", "大安區")}}}
	f.h = NewWebhookHandlerWithOptions(f.api.client, nil, weather.NewLookup(repo, nil, 60), f.alert, f.conv, geo, repo)
	if withAI {
		f.h = f.h.WithAssistant(assistant.New(&assistant.Config{Completer: f.qa}), nil).WithIntent(intent.New(f.ai))
	}
	return f
}

func (f *intentFixture) clarifyState(t *testing.T) *conversation.State {
	t.Helper()
	st, err := f.conv.Get(context.Background(), "U1")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// [F38-02] 單打「天母」：1 筆 Area 結果也出選項卡（不直接回天氣）、狀態為 TypeLocationClarify、AI 0 次.
func TestBarePlaceTianmuShowsCard(t *testing.T) {
	geo := bareGeo()
	f := newBareFixture(t, geo, answerJSON("none", "", "high"), true)
	reply := f.say(t, "天母")
	mustContain(t, reply, candidateConfirmAltText, "台北市士林區")
	if strings.Contains(reply, "降雨機率") {
		t.Errorf("a bare place must show the card, not the forecast: %s", reply)
	}
	st := f.clarifyState(t)
	if st == nil || st.Type != conversation.TypeLocationClarify || len(st.Candidates) != 1 {
		t.Fatalf("state = %+v, want TypeLocationClarify with 1 candidate", st)
	}
	if f.ai.count() != 0 || len(f.qa.asked) != 0 {
		t.Errorf("intent AI = %d, QA = %d, want 0/0", f.ai.count(), len(f.qa.asked))
	}
	if !slices.Equal(geo.calls, []string{"天母"}) {
		t.Errorf("geocoder calls = %v, want [天母]", geo.calls)
	}
}

// [F38-02] 單打「陽明山」：北投區（Area）與金湖鎮（只有公車站）→ 卡片只有北投區.
func TestBarePlaceYangmingsanDropsNonAreaCandidates(t *testing.T) {
	f := newBareFixture(t, bareGeo(), answerJSON("none", "", "high"), true)
	reply := f.say(t, "陽明山")
	mustContain(t, reply, "台北市北投區")
	if strings.Contains(reply, "金湖鎮") {
		t.Errorf("a stop-only candidate must not be a button: %s", reply)
	}
	if st := f.clarifyState(t); st == nil || len(st.Candidates) != 1 || st.Candidates[0].District != "北投區" {
		t.Errorf("candidates = %+v, want only 北投區", st)
	}
	if f.ai.count() != 0 {
		t.Errorf("intent AI calls = %d, want 0", f.ai.count())
	}
}

// [F38-02] 單打「日月潭」→ 卡片魚池鄉；按鈕文字「南投縣魚池鄉」走既有選地點流程回天氣.
func TestBarePlaceRiyuetanCardThenButtonReturnsForecast(t *testing.T) {
	f := newBareFixture(t, bareGeo(), answerJSON("none", "", "high"), true)
	mustContain(t, f.say(t, "日月潭"), "南投縣魚池鄉")
	mustContain(t, f.say(t, "南投縣魚池鄉"), "降雨機率")
	if st := f.clarifyState(t); st != nil {
		t.Errorf("state must be cleared after the choice, got %+v", st)
	}
}

// [F38-02] 結果全非 Area（幸福只有車站）或 0 筆（謝謝）→ 不出卡、交給問答（AI 啟用）.
func TestBarePlaceNonAreaFallsThroughToQA(t *testing.T) {
	for _, word := range []string{"幸福", "謝謝"} {
		t.Run(word, func(t *testing.T) {
			geo := bareGeo()
			f := newBareFixture(t, geo, answerJSON("none", "", "high"), true)
			reply := f.say(t, word)
			if strings.Contains(reply, "請確認") || strings.Contains(reply, "請選擇") {
				t.Errorf("no card expected: %s", reply)
			}
			if len(f.qa.asked) != 1 {
				t.Errorf("QA calls = %d, want 1", len(f.qa.asked))
			}
			if !slices.Equal(geo.calls, []string{word}) {
				t.Errorf("geocoder calls = %v, want [%s]", geo.calls, word)
			}
			if st := f.clarifyState(t); st != nil {
				t.Errorf("no state expected, got %+v", st)
			}
		})
	}
}

// [F38-02] AI 停用時，非 Area 的單打短詞回既有查無句.
func TestBarePlaceNonAreaWithoutAIRepliesNotFound(t *testing.T) {
	f := newBareFixture(t, bareGeo(), "", false)
	mustContain(t, f.say(t, "幸福"), "找不到「幸福」的天氣資料")
}

// [F38-02] 9 字以上不送地名服務；8 字送一次（上限邊界）.
func TestBarePlaceLengthLimit(t *testing.T) {
	geo := bareGeo()
	f := newBareFixture(t, geo, answerJSON("none", "", "high"), true)
	f.say(t, "一二三四五六七八九")
	if len(geo.calls) != 0 {
		t.Errorf("9 runes: geocoder calls = %v, want none", geo.calls)
	}
	f.say(t, "一二三四五六七八")
	if !slices.Equal(geo.calls, []string{"一二三四五六七八"}) {
		t.Errorf("8 runes: geocoder calls = %v, want exactly one", geo.calls)
	}
}

// [F38-02] 官方鄉鎮名「大安區」直接回天氣，地名服務 0 次.
func TestBarePlaceOfficialNameSkipsGeocoder(t *testing.T) {
	geo := bareGeo()
	f := newBareFixture(t, geo, answerJSON("none", "", "high"), true)
	mustContain(t, f.say(t, "大安區"), "降雨機率")
	if len(geo.calls) != 0 {
		t.Errorf("geocoder calls = %v, want none", geo.calls)
	}
}

// [F38-02] 地名服務出錯 → 不出卡、不回錯誤句，照舊落問答.
func TestBarePlaceGeocoderErrorFallsThrough(t *testing.T) {
	geo := &bareGeocoder{err: errors.New("nominatim down")}
	f := newBareFixture(t, geo, answerJSON("none", "", "high"), true)
	reply := f.say(t, "天母")
	if strings.Contains(reply, "請確認") || strings.Contains(reply, "發生錯誤") {
		t.Errorf("no card and no error sentence expected: %s", reply)
	}
	if len(f.qa.asked) != 1 || len(geo.calls) != 1 {
		t.Errorf("QA = %d, geocoder calls = %d, want 1/1", len(f.qa.asked), len(geo.calls))
	}
}

// [F38-07] 單打查詢最多等 2 秒：一般短句不能被全服務共用的 Nominatim 限速佇列拖住.
func TestBarePlaceGeocodeHasDeadline(t *testing.T) {
	geo := bareGeo()
	f := newBareFixture(t, geo, answerJSON("none", "", "high"), true)
	mustContain(t, f.say(t, "天母"), "台北市士林區")
	if len(geo.budgets) != 1 || geo.budgets[0] <= 0 || geo.budgets[0] > barePlaceGeocodeTimeout {
		t.Errorf("geocode budgets = %v, want one deadline within %v", geo.budgets, barePlaceGeocodeTimeout)
	}
	if barePlaceGeocodeTimeout > 2*time.Second {
		t.Errorf("barePlaceGeocodeTimeout = %v, want at most 2s", barePlaceGeocodeTimeout)
	}
}

// [F38-07] 每人每天最多送 barePlaceDailyLimit 次地名服務；第 61 次往下（不送），別人不受影響.
func TestBarePlaceDailyLimitPerUser(t *testing.T) {
	geo := bareGeo()
	f := newBareFixture(t, geo, answerJSON("none", "", "high"), true)
	const word = "好喔好喔" // 不在夾具：0 筆、不出卡、不留對話狀態
	for range barePlaceDailyLimit {
		f.say(t, word)
	}
	if len(geo.calls) != barePlaceDailyLimit {
		t.Fatalf("geocoder calls after %d messages = %d", barePlaceDailyLimit, len(geo.calls))
	}
	f.say(t, word)
	if len(geo.calls) != barePlaceDailyLimit {
		t.Errorf("message %d reached the geocoder: calls = %d", barePlaceDailyLimit+1, len(geo.calls))
	}
	mustContain(t, f.sayAs(t, "U2", "天母"), "台北市士林區")
	if len(geo.calls) != barePlaceDailyLimit+1 {
		t.Errorf("U2 must still be looked up: calls = %d, want %d", len(geo.calls), barePlaceDailyLimit+1)
	}
}

// [F38-08] 純數字或符號（驗證碼、電話、「!!」）不送地名服務；含字母的「台北101」照送.
func TestBarePlaceDigitsOnlyNotSent(t *testing.T) {
	geo := bareGeo()
	f := newBareFixture(t, geo, answerJSON("none", "", "high"), true)
	for _, text := range []string{"123456", "0912", "!!", "12/25"} {
		f.say(t, text)
	}
	if len(geo.calls) != 0 {
		t.Errorf("digits or symbols reached the geocoder: %v", geo.calls)
	}
	f.say(t, "台北101")
	if !slices.Equal(geo.calls, []string{"台北101"}) {
		t.Errorf("geocoder calls = %v, want [台北101]", geo.calls)
	}
}

// [F38-02] 記餐與固定指令不進單打路徑：地名服務 0 次.
func TestBarePlaceMealAndKeywordSkipGeocoder(t *testing.T) {
	geo := bareGeo()

	mf := newMealFixture(t, noodleText)
	mf.h.geocoder = geo
	mustContain(t, mf.say(t, "午餐吃了牛肉麵"), "紅燒牛肉麵")

	ff := newFitnessFixture(t)
	ff.h.geocoder = geo
	mustContain(t, ff.say(t, "今日統計"), "今日熱量收支")

	if len(geo.calls) != 0 {
		t.Errorf("geocoder calls = %v, want none", geo.calls)
	}
}

// [F38-02] 含意圖提示字的「日月潭天氣」：原文不先送地名服務，只送意圖 AI 抽出的「日月潭」一次（清單恰為 [日月潭]）.
func TestBarePlaceHintWordTextIsNotGeocodedRaw(t *testing.T) {
	geo := bareGeo()
	f := newBareFixture(t, geo, answerJSON("weather_query", "日月潭", "high"), true)
	mustContain(t, f.say(t, "日月潭天氣"), "降雨機率")
	if !slices.Equal(geo.calls, []string{"日月潭"}) {
		t.Errorf("geocoder calls = %v, want exactly [日月潭]", geo.calls)
	}
}

// [F38-03] 意圖 AI 抽出「陽明山」、唯一非公車站結果 → 直接回天氣、不出卡（行為不變）.
func TestIntentExtractedPlaceStillRepliesForecastDirectly(t *testing.T) {
	geo := &bareGeocoder{byQuery: map[string][]geocoding.GeocodeResult{"陽明山": {yangmingArea}}}
	f := newBareFixture(t, geo, answerJSON("weather_query", "陽明山", "high"), true)
	reply := f.say(t, "陽明山天氣")
	mustContain(t, reply, "降雨機率")
	if strings.Contains(reply, "請確認") {
		t.Errorf("an AI-extracted unique place must not show a card: %s", reply)
	}
	if !slices.Equal(geo.calls, []string{"陽明山"}) {
		t.Errorf("geocoder calls = %v, want [陽明山]", geo.calls)
	}
}

// [F38-11] 運動詞「健身房」不送地名服務（地名服務對它會回 1 筆非 Area 結果）：回運動教學、geocoder 0 次、問答 0 次.
func TestBarePlaceWorkoutWordSkipsGeocoder(t *testing.T) {
	geo := &bareGeocoder{byQuery: map[string][]geocoding.GeocodeResult{
		"健身房": {{City: "台北市", District: "大安區", DisplayName: "健身房, 大安區, 台北市"}},
	}}
	ff := newFitnessFixture(t)
	ff.h.geocoder = geo
	mustContain(t, ff.say(t, "健身房"), workoutGuideMessage)
	if len(geo.calls) != 0 {
		t.Errorf("geocoder calls = %v, want none for a workout word", geo.calls)
	}
}
