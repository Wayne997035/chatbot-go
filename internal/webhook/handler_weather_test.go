package webhook

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/line"
	"chatbot-go/internal/models"
	"chatbot-go/internal/region"
	weatherstore "chatbot-go/internal/storage/database/weather"
	"chatbot-go/internal/weather"
	"context"
	"encoding/json"
	"fmt"
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

// placeWeatherRepo 記錄 FindByDistrictAndCity 收到的 (district, city)，並依參數回該地 forecast.
// FindByDistrict 對任何輸入回空；不得用 fakeWeatherRepo（它忽略參數，斷言會恆綠）.
type placeWeatherRepo struct {
	districts []weatherstore.DistrictEntry
	calls     [][2]string // {district, city}
}

func (p *placeWeatherRepo) FindByDistrict(context.Context, string) ([]weatherstore.WeatherForecast, error) {
	return nil, nil
}

func (p *placeWeatherRepo) FindByDistrictAndCity(_ context.Context, district, city string) (*weatherstore.WeatherForecast, error) {
	p.calls = append(p.calls, [2]string{district, city})
	return &weatherstore.WeatherForecast{
		City: city, District: district,
		Elements: []weatherstore.ForecastElement{{ElementName: "PoP12h", Description: "12小時降雨機率", Value: "30"}},
	}, nil
}

func (p *placeWeatherRepo) FindAllDistricts(context.Context) ([]weatherstore.DistrictEntry, error) {
	return p.districts, nil
}

func (p *placeWeatherRepo) Upsert(context.Context, *weatherstore.WeatherForecast) error { return nil }

// TestCandidateButtonsSendPlaceNames [F24-86] 按鈕送「縣市+鄉鎮」，缺任一送編號.
func TestCandidateButtonsSendPlaceNames(t *testing.T) {
	candidates := []conversation.CandidateLocation{
		{City: "台北市", District: "松山區", DisplayName: strings.Repeat("臺", 50)},
		{City: "臺北市", District: "中山區"},
		{DisplayName: "某地"},
	}
	wants := []string{"台北市松山區", "臺北市中山區", "3"}

	bubble := buildCandidateFlexMessage(candidates)
	if len(bubble.Footer.Contents) != len(wants) {
		t.Fatalf("buttons = %d, want %d", len(bubble.Footer.Contents), len(wants))
	}
	for i, want := range wants {
		btn, ok := bubble.Footer.Contents[i].(models.ButtonComponent)
		if !ok {
			t.Fatalf("button %d type = %T", i, bubble.Footer.Contents[i])
		}
		if btn.Action.Text != want {
			t.Errorf("button %d text = %q, want %q", i, btn.Action.Text, want)
		}
	}
	first := bubble.Footer.Contents[0].(models.ButtonComponent)
	if n := utf8.RuneCountInString(first.Action.Label); n > 40 {
		t.Errorf("label runes = %d, want <= 40", n)
	}
}

// TestClarifyPicksExactCityAndDistrict [F24-86] 先精確比對縣市+鄉鎮，再編號，最後鄉鎮包含.
func TestClarifyPicksExactCityAndDistrict(t *testing.T) {
	taipeiFirst := []conversation.CandidateLocation{
		{City: "台北市", District: "中山區", DisplayName: "台北市中山區", Source: "fuzzy"},
		{City: "基隆市", District: "中山區", DisplayName: "基隆市中山區", Source: "fuzzy"},
	}
	taipeiSecond := []conversation.CandidateLocation{
		{City: "基隆市", District: "中山區", DisplayName: "基隆市中山區", Source: "fuzzy"},
		{City: "臺北市", District: "中山區", DisplayName: "臺北市中山區", Source: "fuzzy"},
	}
	tests := []struct {
		name         string
		candidates   []conversation.CandidateLocation
		text         string
		wantDistrict string
		wantCity     string
	}{
		{"exact second candidate", taipeiFirst, "基隆市中山區", "中山區", "基隆市"},
		{"index", taipeiFirst, "2", "中山區", "基隆市"},
		{"district only keeps first", taipeiFirst, "中山區", "中山區", "台北市"},
		{"exact with 臺 candidate listed second", taipeiSecond, "台北市中山區", "中山區", "台北市"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &placeWeatherRepo{}
			api := newLineAPI(t, nil)
			h := NewWebhookHandler(api.client, nil, weather.NewLookup(repo, nil, 60), nil, conversation.NewMemoryManager())
			h = h.WithAdvisor(weather.NewAdvisor(tipCompleter{answer: "not json"}, nil))
			h.handleLocationClarify(context.Background(), "U1", tt.text, "tok", &conversation.State{Candidates: tt.candidates})
			if len(repo.calls) != 1 {
				t.Fatalf("weather queries = %v, want exactly one", repo.calls)
			}
			if got := repo.calls[0]; got != [2]string{tt.wantDistrict, tt.wantCity} {
				t.Errorf("queried (district, city) = %v, want (%s, %s)", got, tt.wantDistrict, tt.wantCity)
			}
		})
	}
}

// TestCandidateTapAfterExpiryLooksUpPlace [F24-86] 對話過期後點舊按鈕，送出的地名照一般天氣查詢回答.
func TestCandidateTapAfterExpiryLooksUpPlace(t *testing.T) {
	repo := &placeWeatherRepo{districts: []weatherstore.DistrictEntry{
		{City: "台北市", District: "松山區"}, {City: "台北市", District: "中山區"}, {City: "基隆市", District: "中山區"},
	}}
	api := newLineAPI(t, nil)
	h := NewWebhookHandlerWithOptions(api.client, nil, weather.NewLookup(repo, nil, 60), nil, conversation.NewMemoryManager(), nil, repo)
	h = h.WithAdvisor(weather.NewAdvisor(tipCompleter{answer: "not json"}, nil))

	h.handleTextMessage(context.Background(), "U1", "台北市松山區", "tok")

	if len(repo.calls) != 1 || repo.calls[0] != [2]string{"松山區", "台北市"} {
		t.Fatalf("weather queries = %v, want [(松山區, 台北市)]", repo.calls)
	}
	raw := api.lastReply(t)
	if !strings.Contains(raw, "松山區") {
		t.Errorf("reply lacks 松山區: %s", raw)
	}
	for _, bad := range []string{"找到多個符合的地點", "找不到"} {
		if strings.Contains(raw, bad) {
			t.Errorf("reply must not contain %q: %s", bad, raw)
		}
	}
}

// fuzzyTestHandler 建一個只帶 districts 的 handler 與它的對話管理器.
func fuzzyTestHandler(t *testing.T, districts []weatherstore.DistrictEntry) (*WebhookHandler, *lineAPI, *conversation.Manager) {
	t.Helper()
	repo := &placeWeatherRepo{districts: districts}
	api := newLineAPI(t, nil)
	mgr := conversation.NewMemoryManager()
	h := NewWebhookHandlerWithOptions(api.client, nil, weather.NewLookup(repo, nil, 60), nil, mgr, nil, repo)
	return h, api, mgr
}

func numberedDistricts(n int) []weatherstore.DistrictEntry {
	out := make([]weatherstore.DistrictEntry, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, weatherstore.DistrictEntry{City: "台北市", District: fmt.Sprintf("測試%02d區", i)})
	}
	return out
}

func allRegionDistricts() []weatherstore.DistrictEntry {
	var out []weatherstore.DistrictEntry
	for _, c := range region.Counties {
		for _, d := range region.Townships(c) {
			out = append(out, weatherstore.DistrictEntry{City: c, District: d})
		}
	}
	return out
}

// countFooterButtons 數最後一則回覆（Flex）footer 的按鈕數.
func countFooterButtons(t *testing.T, raw string) int {
	t.Helper()
	var payload struct {
		Messages []struct {
			Contents struct {
				Footer struct {
					Contents []json.RawMessage `json:"contents"`
				} `json:"footer"`
			} `json:"contents"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil || len(payload.Messages) == 0 {
		t.Fatalf("parse reply %q: %v", raw, err)
	}
	return len(payload.Messages[0].Contents.Footer.Contents)
}

// TestFuzzyMatchTooManyCandidatesAsksForFullName [F24-89] 超過 40 個回文字、不設對話狀態；40 個以內出卡片.
func TestFuzzyMatchTooManyCandidatesAsksForFullName(t *testing.T) {
	all := allRegionDistricts()
	if len(all) != 368 {
		t.Fatalf("region districts = %d, want 368", len(all))
	}
	areaCount := len(weather.FuzzyMatch(all, "區"))
	if areaCount <= maxCandidateButtons {
		t.Fatalf("fixture invalid: 區 matches %d, want > %d", areaCount, maxCandidateButtons)
	}

	tests := []struct {
		name        string
		districts   []weatherstore.DistrictEntry
		text        string
		wantCard    bool
		wantButtons int
		wantContain string
	}{
		{"區 over limit asks for full name", all, "區", false, 0, fmt.Sprintf("符合的地點有 %d 個", areaCount)},
		{"高雄市 38 buttons", all, "高雄市", true, 38, ""},
		{"boundary 40 shows card", numberedDistricts(40), "測試", true, 40, ""},
		{"boundary 41 asks for full name", numberedDistricts(41), "測試", false, 0, "符合的地點有 41 個"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, api, mgr := fuzzyTestHandler(t, tt.districts)
			h.handleTextMessage(context.Background(), "U1", tt.text, "tok")
			raw := api.lastReply(t)
			isCard := strings.Contains(raw, "找到多個符合的地點")
			if isCard != tt.wantCard {
				t.Fatalf("card = %v, want %v; reply = %.200s", isCard, tt.wantCard, raw)
			}
			if tt.wantCard {
				if n := countFooterButtons(t, raw); n != tt.wantButtons {
					t.Errorf("buttons = %d, want %d", n, tt.wantButtons)
				}
				return
			}
			if !strings.Contains(raw, "請打得更完整") || !strings.Contains(raw, tt.wantContain) {
				t.Errorf("reply %q missing 請打得更完整 or %q", raw, tt.wantContain)
			}
			if st, _ := mgr.Get(context.Background(), "U1"); st != nil {
				t.Errorf("conversation state = %+v, want none", st)
			}
		})
	}
}

// TestCandidateFlexFitsLineBubbleLimit [F24-89][F241-03] 長名稱經 label 截斷後，40 顆候選的卡片仍在 30 KB 內.
func TestCandidateFlexFitsLineBubbleLimit(t *testing.T) {
	cands := make([]conversation.CandidateLocation, 0, maxCandidateButtons)
	for i := 0; i < maxCandidateButtons; i++ {
		cands = append(cands, conversation.CandidateLocation{
			City:        "台北市",
			District:    fmt.Sprintf("測試%02d區", i),
			DisplayName: strings.Repeat("地", 300), // Nominatim display_name 是完整地址，遠超 label 上限 40 字
		})
	}
	data, err := json.Marshal(buildCandidateFlexMessage(cands))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 30*1024 {
		t.Errorf("bubble JSON = %d bytes, want <= %d", len(data), 30*1024)
	}
}
