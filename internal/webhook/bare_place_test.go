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
	lukangPlace   = geocoding.GeocodeResult{City: "彰化縣", District: "鹿港鎮", DisplayName: "彰化縣鹿港鎮"}
	shilinOffice  = geocoding.GeocodeResult{City: "台北市", District: "士林區", DisplayName: "台北市士林區"}
	// [F39-18] 「北投」Lead 實測回 3 筆 Area(北投區、淡水區、草屯鎮),取 2 筆:2 個以上區域類選項才出卡.
	beitouArea = geocoding.GeocodeResult{City: "台北市", District: "北投區", DisplayName: "北投, 北投區, 台北市", Area: true}
	tamsuiArea = geocoding.GeocodeResult{City: "新北市", District: "淡水區", DisplayName: "北投, 淡水區, 新北市", Area: true}
)

func bareGeo() *bareGeocoder {
	return &bareGeocoder{byQuery: map[string][]geocoding.GeocodeResult{
		"天母":  {tianmuArea},
		"陽明山": {yangmingArea, kinmenStop},
		"日月潭": {riyuetanArea},
		"北投":  {beitouArea, tamsuiArea},
		"幸福":  {xingfuStation},
		"鹿港":  {lukangPlace},
		// [F38-09] 單一非 Area 結果，區名「士林區」就在使用者原文裡（驗證「問 士林區公所」不被當成選了士林區）.
		"士林區公所": {shilinOffice},
		// [F38-09] 多筆非 Area 結果（Lead 實測「辦公室」回 4 筆建築，取 2 筆）：沒有區域景點類時卡上要列全部.
		"辦公室": {
			{City: "台北市", District: "信義區", DisplayName: "辦公室, 信義區, 台北市"},
			{City: "台北市", District: "北投區", DisplayName: "辦公室, 北投區, 台北市"},
		},
	}}
}

// [F38-09] 沒有區域景點類結果時列「全部」結果，不是只列第一筆：卡上兩區都在、對話狀態候選 2 筆、有「不是要查天氣」.
func TestBarePlaceNonAreaMultipleResultsListsAll(t *testing.T) {
	f := newBareFixture(t, bareGeo(), answerJSON("none", "", "high"), true)
	mustContain(t, f.say(t, "辦公室"), "台北市信義區", "台北市北投區", notWeatherLabel)
	if st := f.clarifyState(t); st == nil || len(st.Candidates) != 2 {
		t.Fatalf("state = %+v, want TypeLocationClarify with both candidates", st)
	}
}

// [F38-09] AI 問答停用時不放「不是要查天氣」：點了只會回天氣查無句，與按鈕的意思相反.
func TestBarePlaceNoNotWeatherButtonWithoutAI(t *testing.T) {
	f := newBareFixture(t, bareGeo(), "", false)
	reply := f.say(t, "鹿港")
	mustContain(t, reply, "彰化縣鹿港鎮")
	if strings.Contains(reply, notWeatherLabel) {
		t.Errorf("no QA to fall back to, so no not-weather button: %s", reply)
	}
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

// [F39-18] 單打地名卡上只剩 1 個區域類選項就沒得選:直接回該區天氣,不出卡、不留選地點狀態、AI 與問答 0 次.
// 天母 → 台北市士林區;陽明山(北投區 Area + 金湖鎮只有公車站)→ 台北市北投區,不提金湖鎮;日月潭 → 南投縣魚池鄉.
func TestBarePlaceSingleAreaRepliesForecastDirectly(t *testing.T) {
	for _, tc := range []struct{ word, header, absent string }{
		{"天母", "台北市 士林區", ""},
		{"陽明山", "台北市 北投區", "金湖鎮"},
		{"日月潭", "南投縣 魚池鄉", ""},
	} {
		t.Run(tc.word, func(t *testing.T) {
			geo := bareGeo()
			f := newBareFixture(t, geo, answerJSON("none", "", "high"), true)
			reply := f.say(t, tc.word)
			mustContain(t, reply, tc.header, "降雨機率")
			if strings.Contains(reply, candidateConfirmAltText) || strings.Contains(reply, notWeatherLabel) {
				t.Errorf("a single area choice must not show the card: %s", reply)
			}
			if tc.absent != "" && strings.Contains(reply, tc.absent) {
				t.Errorf("a stop-only result must not show up: %s", reply)
			}
			if st := f.clarifyState(t); st != nil {
				t.Errorf("no clarify state expected, got %+v", st)
			}
			if f.ai.count() != 0 || len(f.qa.asked) != 0 {
				t.Errorf("intent AI = %d, QA = %d, want 0/0", f.ai.count(), len(f.qa.asked))
			}
			if !slices.Equal(geo.calls, []string{tc.word}) {
				t.Errorf("geocoder calls = %v, want [%s]", geo.calls, tc.word)
			}
		})
	}
}

// [F39-18] 2 個以上區域類選項照舊出卡(北投 → 北投區、淡水區);按鈕文字「台北市北投區」走既有選地點流程回天氣、狀態清除.
func TestBarePlaceMultiAreaCardThenButtonReturnsForecast(t *testing.T) {
	f := newBareFixture(t, bareGeo(), answerJSON("none", "", "high"), true)
	reply := f.say(t, "北投")
	mustContain(t, reply, "找到多個符合的地點，請選擇", "台北市北投區", "新北市淡水區")
	if strings.Contains(reply, "降雨機率") {
		t.Errorf("two area choices must show the card, not a forecast: %s", reply)
	}
	if st := f.clarifyState(t); st == nil || st.Type != conversation.TypeLocationClarify || len(st.Candidates) != 2 {
		t.Fatalf("state = %+v, want TypeLocationClarify with 2 candidates", st)
	}
	mustContain(t, f.say(t, "台北市北投區"), "台北市 北投區", "降雨機率")
	if st := f.clarifyState(t); st != nil {
		t.Errorf("state must be cleared after the choice, got %+v", st)
	}
}

// failingCityRepo 依城市與行政區查天氣時回錯誤(err 非 nil)或查無資料(err 為 nil),其餘照 namedWeatherRepo.
type failingCityRepo struct {
	namedWeatherRepo
	err error
}

func (r *failingCityRepo) FindByDistrictAndCity(context.Context, string, string) (*weatherstore.WeatherForecast, error) {
	return nil, r.err
}

// [F39-18] 地名服務唯一結果直接回天氣時(單打唯一區域類選項、意圖路徑唯一結果),查天氣失敗或查無資料要回對應的錯誤句,
// 不出卡、不留選地點狀態、不落到問答.
func TestGeocodeForecastLookupFailureReplies(t *testing.T) {
	for _, tc := range []struct {
		name, text, answer string
		err                error
		want               string
	}{
		{"bare place, lookup error", "天母", answerJSON("none", "", "high"), errors.New("mongo down"), "查詢天氣資料時發生錯誤"},
		{"bare place, no forecast", "天母", answerJSON("none", "", "high"), nil, "抱歉找不到該地點的天氣資料"},
		{"intent place, lookup error", "日月潭天氣", answerJSON("weather_query", "日月潭", "high"), errors.New("mongo down"), "查詢天氣資料時發生錯誤"},
		{"intent place, no forecast", "日月潭天氣", answerJSON("weather_query", "日月潭", "high"), nil, "抱歉找不到該地點的天氣資料"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntentFixture(t, tc.answer)
			repo := &failingCityRepo{err: tc.err}
			f.h = NewWebhookHandlerWithOptions(f.api.client, nil, weather.NewLookup(repo, nil, 60), f.alert, f.conv, bareGeo(), repo).
				WithAssistant(assistant.New(&assistant.Config{Completer: f.qa}), nil).WithIntent(intent.New(f.ai))
			reply := f.say(t, tc.text)
			mustContain(t, reply, tc.want)
			if strings.Contains(reply, "降雨機率") || strings.Contains(reply, candidateConfirmAltText) {
				t.Errorf("a failed lookup must not show a forecast or a card: %s", reply)
			}
			if st := f.clarifyState(t); st != nil {
				t.Errorf("no clarify state expected, got %+v", st)
			}
			if len(f.qa.asked) != 0 {
				t.Errorf("QA asked %d times, want 0", len(f.qa.asked))
			}
		})
	}
}

// notWeatherButtonJSON [F38-09] 卡片上「不是要查天氣」按鈕在 JSON 裡的 label 與送出文字.
func notWeatherButtonJSON(original string) string {
	return `"label":"不是要查天氣","text":"問 ` + original + `"`
}

// [F38-09] 單打「鹿港」：地名服務回 1 筆非 Area → 出卡（舊：沒有 Area 回 false 落問答）；卡上有「不是要查天氣」、按鈕送「問 鹿港」；狀態只有 1 筆；問答 0 次.
func TestBarePlaceNonAreaSingleResultShowsCardWithNotWeatherButton(t *testing.T) {
	geo := bareGeo()
	f := newBareFixture(t, geo, answerJSON("none", "", "high"), true)
	reply := f.say(t, "鹿港")
	mustContain(t, reply, "彰化縣鹿港鎮", candidateConfirmAltText, notWeatherButtonJSON("鹿港"))
	st := f.clarifyState(t)
	if st == nil || st.Type != conversation.TypeLocationClarify || len(st.Candidates) != 1 || st.Candidates[0].District != "鹿港鎮" {
		t.Fatalf("state = %+v, want TypeLocationClarify with only 鹿港鎮", st)
	}
	if len(f.qa.asked) != 0 || f.ai.count() != 0 {
		t.Errorf("QA = %d, intent AI = %d, want 0/0", len(f.qa.asked), f.ai.count())
	}
}

// [F38-09] 接著送「問 鹿港」（按鈕送出的字）→ 狀態清除、問答 1 次且問題為「鹿港」.
func TestBarePlaceNotWeatherButtonRoutesToQA(t *testing.T) {
	f := newBareFixture(t, bareGeo(), answerJSON("none", "", "high"), true)
	f.say(t, "鹿港")
	reply := f.say(t, "問 鹿港")
	if strings.Contains(reply, "降雨機率") {
		t.Errorf("the not-weather button must not return a forecast: %s", reply)
	}
	if st := f.clarifyState(t); st != nil {
		t.Errorf("state must be cleared, got %+v", st)
	}
	if len(f.qa.asked) != 1 || !strings.Contains(f.qa.asked[0], "【問題】\n鹿港") {
		t.Errorf("QA asked = %q, want exactly one with question 鹿港", f.qa.asked)
	}
}

// [F38-09] 結果全非 Area（幸福只有車站）→ 出卡（舊 TestBarePlaceNonAreaFallsThroughToQA：落問答）；0 筆（謝謝）→ 不出卡、落問答.
func TestBarePlaceAllNonAreaShowsCardAndZeroResultFallsToQA(t *testing.T) {
	t.Run("幸福 shows a card", func(t *testing.T) {
		f := newBareFixture(t, bareGeo(), answerJSON("none", "", "high"), true)
		mustContain(t, f.say(t, "幸福"), "台東縣關山鎮", notWeatherButtonJSON("幸福"))
		if len(f.qa.asked) != 0 {
			t.Errorf("QA calls = %d, want 0", len(f.qa.asked))
		}
		if st := f.clarifyState(t); st == nil || len(st.Candidates) != 1 {
			t.Errorf("state = %+v, want 1 candidate", st)
		}
	})
	t.Run("謝謝 falls to QA", func(t *testing.T) {
		geo := bareGeo()
		f := newBareFixture(t, geo, answerJSON("none", "", "high"), true)
		reply := f.say(t, "謝謝")
		if strings.Contains(reply, "請確認") || strings.Contains(reply, "請選擇") || strings.Contains(reply, "不是要查天氣") {
			t.Errorf("no card expected: %s", reply)
		}
		if len(f.qa.asked) != 1 {
			t.Errorf("QA calls = %d, want 1", len(f.qa.asked))
		}
		if !slices.Equal(geo.calls, []string{"謝謝"}) {
			t.Errorf("geocoder calls = %v, want [謝謝]", geo.calls)
		}
		if st := f.clarifyState(t); st != nil {
			t.Errorf("no state expected, got %+v", st)
		}
	})
}

// [F38-09][F39-18] Area 卡(2 個以上區域類選項才出,北投)也有「不是要查天氣」;這顆按鈕不是候選.
func TestBarePlaceAreaCardsAlsoHaveNotWeatherButton(t *testing.T) {
	f := newBareFixture(t, bareGeo(), answerJSON("none", "", "high"), true)
	mustContain(t, f.say(t, "北投"), "台北市北投區", "新北市淡水區", notWeatherButtonJSON("北投"))
	if st := f.clarifyState(t); st == nil || len(st.Candidates) != 2 {
		t.Errorf("candidates = %+v, want 北投區 and 淡水區 only (the not-weather button is not a candidate)", st)
	}
}

// [F38-09] 意圖路徑（「X天氣」→ handleGeocodingFallback）的多筆卡沒有「不是要查天氣」.
func TestIntentPathCandidateCardHasNoNotWeatherButton(t *testing.T) {
	geo := &bareGeocoder{byQuery: map[string][]geocoding.GeocodeResult{"中山": {yangmingArea, tianmuArea}}}
	f := newBareFixture(t, geo, answerJSON("weather_query", "中山", "high"), true)
	reply := f.say(t, "中山天氣")
	mustContain(t, reply, "台北市北投區", "台北市士林區")
	if strings.Contains(reply, "不是要查天氣") {
		t.Errorf("the intent-path card must not have the not-weather button: %s", reply)
	}
	if !slices.Equal(geo.calls, []string{"中山"}) {
		t.Errorf("geocoder calls = %v, want [中山]", geo.calls)
	}
}

// [F38-09] 原文含區名的「士林區公所」：卡上有「不是要查天氣」；按鈕送的「問 士林區公所」不會被當成選了士林區（不回天氣）、狀態清除、問答 1 次.
func TestBarePlaceNotWeatherButtonNotEatenByDistrictNameInText(t *testing.T) {
	f := newBareFixture(t, bareGeo(), answerJSON("none", "", "high"), true)
	mustContain(t, f.say(t, "士林區公所"), "台北市士林區", notWeatherButtonJSON("士林區公所"))
	reply := f.say(t, "問 士林區公所")
	if strings.Contains(reply, "降雨機率") {
		t.Errorf("the not-weather button must not be read as picking 士林區: %s", reply)
	}
	if st := f.clarifyState(t); st != nil {
		t.Errorf("state must be cleared, got %+v", st)
	}
	if len(f.qa.asked) != 1 || !strings.Contains(f.qa.asked[0], "【問題】\n士林區公所") {
		t.Errorf("QA asked = %q, want exactly one with question 士林區公所", f.qa.asked)
	}
}

// [F38-09] AI 停用時，0 筆的詞回 F38-10 的查無句（舊 TestBarePlaceNonAreaWithoutAIRepliesNotFound 用幸福，現在幸福出卡）.
func TestBarePlaceZeroResultWithoutAIRepliesNotFound(t *testing.T) {
	f := newBareFixture(t, bareGeo(), "", false)
	mustContain(t, f.say(t, "謝謝"), "找不到「謝謝」的天氣資料，請改打所在鄉鎮區（如：大安區）或直接傳送位置")
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
	mustContain(t, f.say(t, "天母"), "台北市 士林區") // [F39-18] 唯一區域類選項直接回天氣
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
	mustContain(t, f.sayAs(t, "U2", "天母"), "台北市 士林區") // [F39-18] 唯一區域類選項直接回天氣
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
