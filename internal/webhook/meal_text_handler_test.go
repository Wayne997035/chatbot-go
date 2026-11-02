package webhook

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/models"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/weather"
	"context"
	"strings"
	"testing"
	"time"
)

// mealCompleter 文字估算依序回答 texts；看圖一律回答 image.
type mealCompleter struct {
	texts []string
	image string
	reqs  []*ai.Request
}

func (c *mealCompleter) Complete(_ context.Context, req *ai.Request) ai.Result {
	c.reqs = append(c.reqs, req)
	answer := c.image
	if len(req.Images) == 0 {
		if len(c.texts) == 0 {
			return ai.Result{Errors: []string{"groq(m) 429: org_secret111111"}}
		}
		answer, c.texts = c.texts[0], c.texts[1:]
	}
	if !req.Accept(answer) {
		return ai.Result{Errors: []string{"groq(m) bad answer"}}
	}
	return ai.Result{Text: answer}
}

func (c *mealCompleter) last() *ai.Request { return c.reqs[len(c.reqs)-1] }

type mealFixture struct {
	h     *WebhookHandler
	api   *lineAPI
	meals *fakeMealRepo
	ai    *mealCompleter
	qa    *qaCompleter
}

func newMealFixture(t *testing.T, texts ...string) *mealFixture {
	t.Helper()
	f := &mealFixture{
		api:   newLineAPI(t, append(jpegHeader, make([]byte, 64)...)),
		meals: &fakeMealRepo{},
		ai:    &mealCompleter{texts: texts},
		qa:    &qaCompleter{answer: "好的"},
	}
	f.h = NewWebhookHandler(nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil, conversation.NewMemoryManager()).
		WithNutrition(nutrition.NewAnalyzer(f.ai), f.meals, nil).
		WithAssistant(assistant.New(&assistant.Config{Completer: f.qa}), nil)
	return f
}

// checked 每則回覆都檢查 LINE 的長度與按鈕限制.
func (f *mealFixture) checked(t *testing.T, input string) string {
	t.Helper()
	assertLINELimits(t, input, f.api.replies[len(f.api.replies)-1])
	return f.api.lastReply(t)
}

func (f *mealFixture) say(t *testing.T, text string) string {
	t.Helper()
	f.h.handleTextMessage(context.Background(), "U1", text, "tok")
	return f.checked(t, text)
}

func (f *mealFixture) photo(t *testing.T, answer string) string {
	t.Helper()
	f.ai.image = answer
	f.h.handleMessage(context.Background(), imageEvent())
	return f.checked(t, "(photo)")
}

func (f *mealFixture) location(t *testing.T, title, address string) string {
	t.Helper()
	f.h.handleMessage(context.Background(), &models.Event{
		Type: "message", ReplyToken: "tok", Source: models.Source{UserID: "U1"},
		Message: models.Message{ID: "9", Type: "location", Title: title, Address: address, Latitude: 25.03, Longitude: 121.56},
	})
	return f.checked(t, "(location)")
}

func mustContain(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in: %s", w, got)
		}
	}
}

const (
	noodleText = `{"is_food":true,"meal_type":"lunch","foods":[{"name":"紅燒牛肉麵","portion":"1碗","calories":650,"protein_g":32,"carbs_g":80,"fat_g":20}],` +
		`"store":null,"advice":"湯頭偏鹹，記得多喝水"}`
	notFoodText   = `{"is_food":false,"foods":[]}`
	bentoClarify  = `{"is_food":true,"meal_type":"dinner","clarify":{"question":"便當差很多，是哪一種？","options":["雞腿便當","排骨便當"]},"foods":[]}`
	porkBentoText = `{"is_food":true,"meal_type":"","foods":[{"name":"排骨便當","portion":"1個","calories":800,"protein_g":30,"carbs_g":100,"fat_g":30}]}`
	eggText       = `{"is_food":true,"foods":[{"name":"滷蛋","portion":"1顆","calories":75,"protein_g":7,"carbs_g":1,"fat_g":5}]}`
	bigMacText    = `{"is_food":true,"foods":[{"name":"大麥克","portion":"1個","calories":530}],"store":{"name":"麥當勞","from":"dish"}}`
	noodlePhoto   = `{"kind":"food","foods":[{"name":"清燉牛肉麵","portion":"1碗","calories":520,"protein_g":35,"carbs_g":70,"fat_g":12}],` +
		`"advice":"清燉比紅燒清爽","store":"林東芳"}`
	bentoPhoto = `{"kind":"food","foods":[{"name":"雞腿便當","portion":"1個","calories":750}],"store":""}`
)

func TestTypedMealIsLoggedWithFollowUpButtons(t *testing.T) {
	f := newMealFixture(t, noodleText)

	reply := f.say(t, "午餐吃了牛肉麵")

	if len(f.meals.meals) != 1 {
		t.Fatalf("meals = %+v", f.meals.meals)
	}
	m := f.meals.meals[0]
	if m.Source != meal.SourceText || m.MealType != "lunch" || m.Note != "午餐吃了牛肉麵" || m.Total.Calories != 650 || m.Place != nil {
		t.Errorf("meal = %+v", m)
	}
	mustContain(t, reply, "已記錄午餐（文字估算，拍照會更準）", "紅燒牛肉麵 1碗：650 大卡", "建議：湯頭偏鹹", "今日累計：650 大卡（1 餐）",
		`"text":"只吃一半"`, `"text":"大份"`, `"label":"補照片"`, `"type":"camera"`, `"text":"在哪吃的"`, `"text":"取消這筆"`)
	if len(f.ai.reqs) != 1 || len(f.ai.last().Images) != 0 || f.api.loading != 1 || len(f.qa.asked) != 0 {
		t.Errorf("ai reqs=%d loading=%d qa=%d", len(f.ai.reqs), f.api.loading, len(f.qa.asked))
	}
}

func TestQuestionsAndIdiomsAreNotLogged(t *testing.T) {
	f := newMealFixture(t, notFoodText)

	f.say(t, "晚餐吃什麼比較好？")
	if len(f.ai.reqs) != 0 || len(f.qa.asked) != 1 {
		t.Errorf("a question goes straight to Q&A: estimates=%d qa=%d", len(f.ai.reqs), len(f.qa.asked))
	}
	f.say(t, "今天吃了閉門羹")
	if len(f.ai.reqs) != 1 || len(f.qa.asked) != 2 || len(f.meals.meals) != 0 {
		t.Errorf("an idiom is checked, not logged, then answered: estimates=%d qa=%d meals=%d", len(f.ai.reqs), len(f.qa.asked), len(f.meals.meals))
	}
}

func TestVagueMealAsksThenLogsWithTheAnswer(t *testing.T) {
	f := newMealFixture(t, bentoClarify, porkBentoText)

	reply := f.say(t, "晚餐吃便當")
	mustContain(t, reply, "便當差很多，是哪一種？", `"text":"雞腿便當"`, `"text":"排骨便當"`, `"label":"拍給我看"`, `"text":"取消"`)
	if len(f.meals.meals) != 0 {
		t.Fatal("nothing is saved before the answer")
	}

	reply = f.say(t, "排骨便當")
	if !strings.Contains(f.ai.last().Prompt, "晚餐吃便當（補充：排骨便當）") || !strings.Contains(f.ai.last().Prompt, "這次不要追問") {
		t.Errorf("follow-up prompt: %s", f.ai.last().Prompt)
	}
	if len(f.meals.meals) != 1 || f.meals.meals[0].MealType != "dinner" || f.meals.meals[0].Total.Calories != 800 {
		t.Fatalf("the meal type from the first message is kept: %+v", f.meals.meals)
	}
	mustContain(t, reply, "已記錄晚餐", "排骨便當")
}

func TestVagueMealAnsweredWithAPhoto(t *testing.T) {
	f := newMealFixture(t, bentoClarify)
	f.say(t, "晚餐吃便當")

	reply := f.photo(t, bentoPhoto)

	if !strings.HasSuffix(f.ai.last().Prompt, "晚餐吃便當") {
		t.Errorf("the typed description is passed to the photo analysis: %q", f.ai.last().Prompt)
	}
	if len(f.meals.meals) != 1 || f.meals.meals[0].Source != meal.SourcePhotoText || f.meals.meals[0].MealType != "dinner" ||
		f.meals.meals[0].Note != "晚餐吃便當" {
		t.Fatalf("meal = %+v", f.meals.meals)
	}
	mustContain(t, reply, "已記錄晚餐", "雞腿便當")
	f.say(t, "排骨便當")
	if len(f.meals.meals) != 1 {
		t.Error("the photo answered the question; the next message is a new topic")
	}
}

func TestClarifyCanBeCancelledOrLeft(t *testing.T) {
	f := newMealFixture(t, bentoClarify, bentoClarify)
	f.say(t, "午餐吃便當")
	if reply := f.say(t, "取消"); !strings.Contains(reply, "這餐先不記錄") || len(f.meals.meals) != 0 {
		t.Errorf("cancel: %s", reply)
	}

	f.say(t, "午餐吃便當")
	if reply := f.say(t, "查天氣"); !strings.Contains(reply, "輸入地名") {
		t.Errorf("a menu tap during the question leaves it and runs the menu item: %s", reply)
	}
	if len(f.ai.reqs) != 2 || len(f.meals.meals) != 0 {
		t.Errorf("no estimate for the menu tap: reqs=%d meals=%d", len(f.ai.reqs), len(f.meals.meals))
	}
}

func TestPhotoAfterTypedMealUpdatesTheSameMeal(t *testing.T) {
	f := newMealFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")

	reply := f.photo(t, noodlePhoto)

	if len(f.meals.meals) != 1 {
		t.Fatalf("the photo must update the typed meal, not add one: %+v", f.meals.meals)
	}
	m := f.meals.meals[0]
	if m.Source != meal.SourcePhotoText || m.Items[0].Name != "清燉牛肉麵" || m.Total.Calories != 520 || m.MealType != "lunch" ||
		m.Place == nil || m.Place.Name != "林東芳" || m.Place.Source != meal.PlaceFromPhoto {
		t.Errorf("meal = %+v place=%+v", m, m.Place)
	}
	if !strings.HasSuffix(f.ai.last().Prompt, "午餐吃了牛肉麵") {
		t.Errorf("hint missing: %q", f.ai.last().Prompt)
	}
	mustContain(t, reply, "已用照片更新剛剛的午餐", "清燉牛肉麵", "📍 林東芳（從照片辨識）", "今日累計：520 大卡（1 餐）")
}

func TestPhotoAfterTheMergeWindowIsANewMeal(t *testing.T) {
	f := newMealFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")
	f.h.recent.Now = func() time.Time { return time.Now().Add(photoMergeWindow + time.Minute) }

	f.photo(t, bentoPhoto)

	if len(f.meals.meals) != 2 || f.meals.meals[1].Source != meal.SourcePhoto || strings.Contains(f.ai.last().Prompt, "牛肉麵") {
		t.Errorf("meals = %+v", f.meals.meals)
	}
}

func TestUserGivenPlaceWinsOverPhoto(t *testing.T) {
	f := newMealFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")
	f.say(t, keywordMealPlace)
	f.say(t, "永康牛肉麵")

	f.photo(t, noodlePhoto)

	if p := f.meals.meals[0].Place; p == nil || p.Name != "永康牛肉麵" || p.Source != meal.PlaceFromUser {
		t.Errorf("place = %+v", p)
	}
}

func TestPortionButtonsAreRelativeToTheEstimate(t *testing.T) {
	f := newMealFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")

	reply := f.say(t, keywordHalf)
	mustContain(t, reply, "已改成一半的份量", "合計：325 大卡", "（份量 ×0.5）", `"text":"原本份量"`)
	if strings.Contains(reply, `"text":"只吃一半"`) {
		t.Error("after changing the portion, offer going back instead")
	}
	steps := []struct {
		text string
		want float64
	}{{keywordHalf, 325}, {keywordOriginal, 650}, {keywordLarge, 975}, {"兩份", 1300}}
	for _, s := range steps {
		f.say(t, s.text)
		if got := f.meals.meals[0].Total.Calories; got != s.want {
			t.Errorf("after %s: %v, want %v", s.text, got, s.want)
		}
	}
}

func TestMealCommandsWithoutARecentMeal(t *testing.T) {
	f := newMealFixture(t)
	for _, text := range []string{keywordHalf, keywordCancelMeal, keywordMealPlace} {
		if reply := f.say(t, text); !strings.Contains(reply, "找不到 30 分鐘內記錄的餐點") {
			t.Errorf("%s: %s", text, reply)
		}
	}
}

func TestCancelRemovesTheRecentMealNotTheLatest(t *testing.T) {
	f := newMealFixture(t, `{"is_food":true,"meal_type":"dinner","day_offset":-1,"foods":[{"name":"火鍋","calories":1200}]}`)
	f.photo(t, bentoPhoto)
	reply := f.say(t, "昨天晚餐吃了火鍋")
	mustContain(t, reply, "已記錄昨天的晚餐")
	// 比較台灣日期，不比較經過幾小時（凌晨跑測試時「昨天 18:30」離現在不到 12 小時）.
	yesterday := time.Now().In(nutrition.Taipei).AddDate(0, 0, -1).Format("2006-01-02")
	if y := f.meals.meals[1].EatenAt.In(nutrition.Taipei); y.Format("2006-01-02 15:04") != yesterday+" 18:30" {
		t.Errorf("yesterday's dinner goes to %s 18:30, got %v", yesterday, y)
	}

	reply = f.say(t, keywordCancelMeal)

	if len(f.meals.meals) != 1 || f.meals.meals[0].Items[0].Name != "雞腿便當" {
		t.Fatalf("today's photo must stay: %+v", f.meals.meals)
	}
	mustContain(t, reply, "已取消這筆紀錄", "今日累計：750 大卡（1 餐）")
	if reply := f.say(t, keywordCancelMeal); !strings.Contains(reply, "找不到") {
		t.Errorf("canceling twice: %s", reply)
	}
}

func TestAddingToTheRecentMeal(t *testing.T) {
	f := newMealFixture(t, noodleText, eggText)
	f.say(t, "午餐吃了牛肉麵")
	f.say(t, keywordHalf)

	reply := f.say(t, "再加一顆滷蛋")

	if len(f.meals.meals) != 1 || len(f.meals.meals[0].Items) != 2 || f.meals.meals[0].Total.Calories != 400 {
		t.Fatalf("the egg is added to the halved noodles: %+v", f.meals.meals)
	}
	if !strings.HasSuffix(f.ai.last().Prompt, "吃了一顆滷蛋") || !strings.Contains(f.ai.last().Prompt, "這次不要追問") {
		t.Errorf("prompt: %q", f.ai.last().Prompt)
	}
	mustContain(t, reply, "已加到剛剛的午餐", "滷蛋 1顆：75 大卡", "合計：400 大卡")
	if f.meals.meals[0].CurrentScale() != 1 {
		t.Error("adding an item makes the current amounts the new estimate")
	}
}

func TestAddingWithoutARecentMealLogsANewOne(t *testing.T) {
	f := newMealFixture(t, eggText)
	f.say(t, "再加一顆滷蛋")
	if len(f.meals.meals) != 1 || f.meals.meals[0].Items[0].Name != "滷蛋" {
		t.Errorf("meals = %+v", f.meals.meals)
	}
	f.say(t, "再加油")
	if len(f.ai.reqs) != 1 || len(f.meals.meals) != 1 {
		t.Error("「再加油」is not food")
	}
}

func TestMealPlaceByTypingOrByLocation(t *testing.T) {
	f := newMealFixture(t, noodleText, noodleText)
	f.say(t, "午餐吃了牛肉麵")
	reply := f.say(t, keywordMealPlace)
	mustContain(t, reply, "在哪間店吃的呢", `"type":"location"`, `"text":"略過"`)

	reply = f.say(t, "鼎泰豐")
	if p := f.meals.meals[0].Place; p == nil || p.Name != "鼎泰豐" || p.Source != meal.PlaceFromUser {
		t.Fatalf("typed place = %+v", p)
	}
	mustContain(t, reply, "已記下這餐在「鼎泰豐」")

	f.say(t, "晚餐吃了牛肉麵")
	f.say(t, keywordMealPlace)
	reply = f.location(t, "鼎泰豐 信義店", "台北市信義區松高路19號")
	if p := f.meals.meals[1].Place; p == nil || p.Name != "鼎泰豐 信義店" || p.Address != "台北市信義區松高路19號" {
		t.Fatalf("location place = %+v", p)
	}
	mustContain(t, reply, "已記下這餐在「鼎泰豐 信義店」")
}

func TestPinWithoutAStoreKeepsOnlyTheAddress(t *testing.T) {
	f := newMealFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")
	f.say(t, keywordMealPlace)
	f.location(t, "位置資訊", "台北市大安區")
	if p := f.meals.meals[0].Place; p == nil || p.Name != "" || p.Address != "台北市大安區" {
		t.Errorf("place = %+v", p)
	}
}

func TestLocationOutsideThePlaceQuestionIsAWeatherLookup(t *testing.T) {
	f := newMealFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")
	f.location(t, "鼎泰豐 信義店", "台北市信義區松高路19號")
	if f.meals.meals[0].Place != nil {
		t.Error("a location shared without being asked must not be attached to the meal")
	}
}

func TestStoreFromASignatureDishIsMarkedAsAGuess(t *testing.T) {
	f := newMealFixture(t, bigMacText)
	reply := f.say(t, "午餐吃了大麥克")
	mustContain(t, reply, "📍 麥當勞（從餐點推測）")
	if strings.Contains(reply, `"text":"在哪吃的"`) {
		t.Error("a meal that already has a place does not ask where")
	}
}

func TestLongMessagesReachTheAssistant(t *testing.T) {
	f := newMealFixture(t)
	question := "我最近想減重，" + strings.Repeat("平常都外食，", 15) + "請問晚餐怎麼吃比較好？"
	f.say(t, question)
	if len(f.qa.asked) != 1 {
		t.Errorf("a %d-character question must reach the assistant", len([]rune(question)))
	}
	if reply := f.say(t, strings.Repeat("長", maxInputRunes+1)); !strings.Contains(reply, "請精簡到 500 字以內") || len(f.qa.asked) != 1 {
		t.Errorf("too long: %s", reply)
	}
}

func TestBareFoodNameOffersALogButton(t *testing.T) {
	f := newMealFixture(t, noodleText)
	f.qa.answer = "牛肉麵一碗約 550 到 700 大卡。\n[[MEAL]]"

	reply := f.say(t, "牛肉麵")
	mustContain(t, reply, "約 550 到 700 大卡", `"label":"🍽 記成一餐"`, `"text":"記錄：牛肉麵"`)
	if strings.Contains(reply, "[[MEAL]]") {
		t.Fatal("marker leaked to the user")
	}

	f.say(t, "記錄：牛肉麵")
	if len(f.meals.meals) != 1 || f.meals.meals[0].Note != "牛肉麵" {
		t.Errorf("the button logs the food: %+v", f.meals.meals)
	}
}

func TestExplicitLogWithoutAI(t *testing.T) {
	api := newLineAPI(t, nil)
	h := NewWebhookHandler(nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil, conversation.NewMemoryManager()).
		WithNutrition(nil, &fakeMealRepo{}, nil)
	h.handleTextMessage(context.Background(), "U1", "記錄：牛肉麵", "tok")
	if !strings.Contains(api.lastReply(t), "飲食估算功能目前未啟用") {
		t.Errorf("reply: %s", api.lastReply(t))
	}
}

func TestEstimateFailureDoesNotLeakErrors(t *testing.T) {
	f := newMealFixture(t, "not json")
	reply := f.say(t, "午餐吃了牛肉麵")
	mustContain(t, reply, "目前無法估算")
	for _, leak := range []string{"429", "org_", "groq"} {
		if strings.Contains(reply, leak) {
			t.Errorf("leak %q: %s", leak, reply)
		}
	}
}
