package webhook

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/line"
	"chatbot-go/internal/models"
	weatherstore "chatbot-go/internal/storage/database/weather"
	"chatbot-go/internal/weather"
	"context"
	"strings"
	"testing"
	"unicode/utf8"
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

// TestButtonLabel [F24-07] 字素單元邊界：40/41/20/21 與國旗、ZWJ、膚色、VS.
func TestButtonLabel(t *testing.T) {
	zh := func(n int) string { return strings.Repeat("臺", n) }
	tests := []struct {
		name, in, want string
	}{
		{"40 chars unchanged", zh(40), zh(40)},
		{"41 chars truncated", zh(41), zh(39) + "…"},
		{"20 emoji unchanged", strings.Repeat("😀", 20), strings.Repeat("😀", 20)},
		{"21 emoji truncated", strings.Repeat("😀", 21), strings.Repeat("😀", 19) + "…"},
		{"flag not split", zh(37) + "🇹🇼" + "x", zh(37) + "…"},
		{"zwj family not split", zh(36) + "👨‍👩‍👧" + "x", zh(36) + "…"},
		{"skin tone kept with base", zh(37) + "👍🏽", zh(37) + "…"},
		{"variation selector kept", zh(37) + "❤️" + "xx", zh(37) + "❤️…"},
		{"variation selector pair dropped together", zh(38) + "❤️" + "xx", zh(38) + "…"},
		{"lone third regional indicator", zh(35) + "🇹🇼🇹x", zh(35) + "🇹🇼…"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buttonLabel(tc.in)
			if got != tc.want {
				t.Errorf("buttonLabel = %q, want %q", got, tc.want)
			}
			if !utf8.ValidString(got) || utf16Len(got) > maxButtonLabelLen {
				t.Errorf("invalid output: valid=%v utf16=%d", utf8.ValidString(got), utf16Len(got))
			}
			if tc.in == got {
				return
			}
			trimmed := []rune(strings.TrimSuffix(got, "…"))
			ri := 0
			for _, r := range trimmed {
				if isRegionalIndicator(r) {
					ri++
				}
			}
			if ri%2 != 0 || (len(trimmed) > 0 && trimmed[len(trimmed)-1] == 0x200D) {
				t.Errorf("truncated output has a dangling character: %q", got)
			}
		})
	}
}

// districtRepo 依傳入的 district 回對應預報；FindByDistrict 查無（讓精確比對落空）.
type districtRepo struct {
	fakeWeatherRepo
	entries []weatherstore.DistrictEntry
}

func (*districtRepo) FindByDistrict(context.Context, string) ([]weatherstore.WeatherForecast, error) {
	return nil, nil
}

func (r *districtRepo) FindByDistrictAndCity(_ context.Context, district, city string) (*weatherstore.WeatherForecast, error) {
	return &weatherstore.WeatherForecast{
		City: city, District: district,
		Elements: []weatherstore.ForecastElement{{ElementName: "PoP12h", Description: "12小時降雨機率", Value: "30"}},
	}, nil
}

func (r *districtRepo) FindAllDistricts(context.Context) ([]weatherstore.DistrictEntry, error) {
	return r.entries, nil
}

type clarifyFixture struct {
	h   *WebhookHandler
	api *lineAPI
}

// newClarifyFixture 用 districtRepo 組出完整 handleTextMessage 流程（含模糊比對）.
func newClarifyFixture(t *testing.T, entries []weatherstore.DistrictEntry) *clarifyFixture {
	t.Helper()
	api := newLineAPI(t, nil)
	repo := &districtRepo{entries: entries}
	h := NewWebhookHandlerWithOptions(api.client, nil, weather.NewLookup(repo, nil, 60), nil, conversation.NewMemoryManager(), nil, repo)
	return &clarifyFixture{h: h, api: api}
}

func (f *clarifyFixture) say(t *testing.T, text string) string {
	t.Helper()
	f.h.handleTextMessage(context.Background(), "U1", text, "tok")
	return f.api.lastReply(t)
}

func setClarifyState(t *testing.T, h *WebhookHandler, candidates []conversation.CandidateLocation) {
	t.Helper()
	state := &conversation.State{Type: conversation.TypeLocationClarify, Candidates: candidates}
	if err := h.convManager.Set(context.Background(), "U1", state); err != nil {
		t.Fatalf("set state: %v", err)
	}
}

func assertNoState(t *testing.T, h *WebhookHandler) {
	t.Helper()
	state, err := h.convManager.Get(context.Background(), "U1")
	if err != nil || state != nil {
		t.Errorf("state should be cleared, got %+v err=%v", state, err)
	}
}

// TestLocationClarifyLetsOtherMessagesThrough [F26-04] 選地點狀態下打別的指令：選單作廢、指令照常執行.
func TestLocationClarifyLetsOtherMessagesThrough(t *testing.T) {
	f := newFitnessFixture(t, "")
	setClarifyState(t, f.h, []conversation.CandidateLocation{
		{City: "宜蘭縣", District: "員山鄉", DisplayName: "員山鄉, 宜蘭縣", Source: "geocode"},
		{City: "宜蘭縣", District: "冬山鄉", DisplayName: "冬山鄉, 宜蘭縣", Source: "geocode"},
	})

	reply := f.say(t, "記運動")

	if !strings.Contains(reply, `"text":"重訓"`) {
		t.Errorf("command was swallowed: %s", reply)
	}
	if strings.Contains(reply, "找到多個符合的地點") {
		t.Errorf("candidate card re-sent: %s", reply)
	}
	assertNoState(t, f.h)
}

// TestLocationClarifyIgnoresCityOnlyCandidateForText [F26-04] District 為空的候選不參與文字比對.
func TestLocationClarifyIgnoresCityOnlyCandidateForText(t *testing.T) {
	f := newFitnessFixture(t, "")
	setClarifyState(t, f.h, []conversation.CandidateLocation{
		{City: "台北市", District: "", DisplayName: "台北市", Source: "geocode"},
		{City: "台北市", District: "北投區", DisplayName: "北投區, 台北市", Source: "geocode"},
	})

	reply := f.say(t, "記運動")

	if !strings.Contains(reply, `"text":"重訓"`) {
		t.Errorf("city-only candidate swallowed the command: %s", reply)
	}
	assertNoState(t, f.h)
}

var xinyiBeitou = []conversation.CandidateLocation{
	{City: "台北市", District: "信義區", DisplayName: "信義區, 台北市", Source: "geocode"},
	{City: "台北市", District: "北投區", DisplayName: "北投區, 台北市", Source: "geocode"},
}

var xinyiBeitouEntries = []weatherstore.DistrictEntry{
	{City: "台北市", District: "信義區"}, {City: "台北市", District: "北投區"},
}

// TestLocationClarifyStillAcceptsNumberAndDistrict [F26-04] 守既有行為：數字與區名仍能選中候選.
func TestLocationClarifyStillAcceptsNumberAndDistrict(t *testing.T) {
	for _, tt := range []struct{ name, text string }{
		{"number", "2"},
		{"district name", "北投"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newClarifyFixture(t, xinyiBeitouEntries)
			setClarifyState(t, f.h, xinyiBeitou)

			reply := f.say(t, tt.text)

			if !strings.Contains(reply, "北投區") || strings.Contains(reply, "信義區") {
				t.Errorf("want the 北投區 forecast, got: %s", reply)
			}
			assertNoState(t, f.h)
		})
	}
}

// TestLocationClarifySingleCharDoesNotSelectCandidate [F26-04] 單字不再反向命中候選（舊行為：「北投區」包含「北」而被選中）.
func TestLocationClarifySingleCharDoesNotSelectCandidate(t *testing.T) {
	f := newClarifyFixture(t, xinyiBeitouEntries)
	setClarifyState(t, f.h, xinyiBeitou)

	reply := f.say(t, "北")

	if strings.Contains(reply, "北投區") {
		t.Errorf("single character selected a candidate: %s", reply)
	}
	assertNoState(t, f.h)
}

// TestSingleCharacterIsNotAPlaceQuery [F26-05] 一個字不查地名；兩個字照常查.
func TestSingleCharacterIsNotAPlaceQuery(t *testing.T) {
	f := newClarifyFixture(t, []weatherstore.DistrictEntry{
		{City: "台北市", District: "大安區"}, {City: "台南市", District: "安南區"}, {City: "台南市", District: "安平區"},
	})

	reply := f.say(t, "安")
	if strings.Contains(reply, "找到多個符合的地點") {
		t.Errorf("single character produced a candidate card: %s", reply)
	}
	// 無結果時的固定說明文字本身含「大安區」當範例，所以大安區看「預報標題」而不是區名.
	for _, leak := range []string{"安南區", "安平區", "台北市 大安區", "12小時降雨機率"} {
		if strings.Contains(reply, leak) {
			t.Errorf("single character matched %q: %s", leak, reply)
		}
	}

	if reply := f.say(t, "大安"); !strings.Contains(reply, "台北市 大安區") {
		t.Errorf("two characters should still be looked up: %s", reply)
	}
}
