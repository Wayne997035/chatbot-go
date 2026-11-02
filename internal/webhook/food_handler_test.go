package webhook

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/models"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// jpegHeader 讓 http.DetectContentType 判定為 image/jpeg.
var jpegHeader = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0}

type lineAPI struct {
	replies []map[string]any
	loading int
	content []byte
}

// newLineAPI 假的 LINE API：/reply、/loading、/content/{id}/content.
func newLineAPI(t *testing.T, content []byte) *lineAPI {
	t.Helper()
	api := &lineAPI{content: content}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/reply":
			data, _ := io.ReadAll(r.Body)
			payload := map[string]any{}
			_ = json.Unmarshal(data, &payload)
			api.replies = append(api.replies, payload)
		case r.URL.Path == "/loading":
			api.loading++
		case strings.HasPrefix(r.URL.Path, "/content/"):
			_, _ = w.Write(api.content)
		}
	}))
	t.Cleanup(srv.Close)
	prev := config.Get()
	config.Set(&config.Config{Line: config.LineConfig{
		ChannelToken: "t", ReplyURL: srv.URL + "/reply", LoadingURL: srv.URL + "/loading", ContentURL: srv.URL + "/content",
	}})
	t.Cleanup(func() { config.Set(prev) })
	return api
}

func (a *lineAPI) lastReply(t *testing.T) string {
	t.Helper()
	if len(a.replies) == 0 {
		t.Fatal("no reply sent")
	}
	raw, _ := json.Marshal(a.replies[len(a.replies)-1])
	return string(raw)
}

type fakeMealRepo struct {
	meals   []meal.Meal
	deleted bool
}

func (f *fakeMealRepo) Insert(_ context.Context, m *meal.Meal) error {
	f.meals = append(f.meals, *m)
	return nil
}

func (f *fakeMealRepo) FindByUserBetween(_ context.Context, userID string, from, to time.Time) ([]meal.Meal, error) {
	var out []meal.Meal
	for i := range f.meals {
		if m := f.meals[i]; m.UserID == userID && !m.EatenAt.Before(from) && m.EatenAt.Before(to) {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeMealRepo) DeleteLatest(_ context.Context, _ string, _ time.Time) (*meal.Meal, error) {
	if len(f.meals) == 0 {
		return nil, nil
	}
	last := f.meals[len(f.meals)-1]
	f.meals = f.meals[:len(f.meals)-1]
	f.deleted = true
	return &last, nil
}

func (f *fakeMealRepo) DeleteAllByUser(_ context.Context, _ string) error {
	f.meals = nil
	return nil
}

type visionCompleter struct {
	answer string
	images int
}

func (v *visionCompleter) Complete(_ context.Context, req *ai.Request) ai.Result {
	v.images += len(req.Images)
	if !req.Accept(v.answer) {
		return ai.Result{Errors: []string{"Groq(m) 429: org_secret123456 limit"}}
	}
	return ai.Result{Text: v.answer}
}

const lunchJSON = `{"kind":"food","foods":[{"name":"雞腿便當","portion":"1個","calories":750,"protein_g":35,"carbs_g":90,"fat_g":25}],"advice":"多補充青菜"}`

func newFoodHandler(answer string, repo *fakeMealRepo) (*WebhookHandler, *visionCompleter) {
	c := &visionCompleter{answer: answer}
	var r meal.MealRepository
	if repo != nil {
		r = repo
	}
	return NewWebhookHandler(nil, nil, nil, nil).WithNutrition(nutrition.NewAnalyzer(c), r, nil), c
}

func imageEvent() *models.Event {
	return &models.Event{
		Type: "message", ReplyToken: "tok", Source: models.Source{UserID: "U1"},
		Message: models.Message{ID: "123", Type: "image", ContentProvider: models.ContentProvider{Type: "line"}},
	}
}

func TestFoodPhotoIsAnalyzedSavedAndSummarized(t *testing.T) {
	api := newLineAPI(t, append(jpegHeader, make([]byte, 64)...))
	repo := &fakeMealRepo{}
	h, c := newFoodHandler(lunchJSON, repo)

	h.handleMessage(context.Background(), imageEvent())

	reply := api.lastReply(t)
	for _, want := range []string{"餐點分析", "雞腿便當 1個：750 大卡", "建議：多補充青菜", "今日累計：750 大卡（1 餐）", `"type":"camera"`, "今日飲食"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q: %s", want, reply)
		}
	}
	if len(repo.meals) != 1 || repo.meals[0].UserID != "U1" || repo.meals[0].Total.Calories != 750 {
		t.Errorf("meal not saved: %+v", repo.meals)
	}
	if c.images != 1 || api.loading != 1 {
		t.Errorf("images=%d loading=%d", c.images, api.loading)
	}
}

func TestFoodPhotoAIFailureShowsPlainMessage(t *testing.T) {
	api := newLineAPI(t, jpegHeader)
	h, _ := newFoodHandler("not json", &fakeMealRepo{})
	h.handleMessage(context.Background(), imageEvent())
	reply := api.lastReply(t)
	if !strings.Contains(reply, "目前無法分析照片") {
		t.Errorf("reply: %s", reply)
	}
	for _, leak := range []string{"429", "org_", "Groq"} {
		if strings.Contains(reply, leak) {
			t.Errorf("AI error leaked to user: %s", reply)
		}
	}
}

func TestNonImageContentIsRejected(t *testing.T) {
	api := newLineAPI(t, []byte("<html>not an image</html>"))
	h, c := newFoodHandler(lunchJSON, &fakeMealRepo{})
	h.handleMessage(context.Background(), imageEvent())
	if !strings.Contains(api.lastReply(t), "照片讀取失敗") || c.images != 0 {
		t.Errorf("reply=%s images=%d", api.lastReply(t), c.images)
	}
}

func TestNotFoodPhoto(t *testing.T) {
	api := newLineAPI(t, jpegHeader)
	h, _ := newFoodHandler(`{"kind":"other","foods":[],"exercise":null}`, &fakeMealRepo{})
	h.handleMessage(context.Background(), imageEvent())
	if !strings.Contains(api.lastReply(t), "不是食物照片") {
		t.Errorf("reply: %s", api.lastReply(t))
	}
}

func TestImageWithoutAI(t *testing.T) {
	api := newLineAPI(t, jpegHeader)
	h := NewWebhookHandler(nil, nil, nil, nil).WithNutrition(nil, nil, nil)
	h.handleMessage(context.Background(), imageEvent())
	if !strings.Contains(api.lastReply(t), "未啟用") || api.loading != 0 {
		t.Errorf("reply: %s", api.lastReply(t))
	}
}

func TestTodayAndDeleteKeywords(t *testing.T) {
	api := newLineAPI(t, nil)
	repo := &fakeMealRepo{meals: []meal.Meal{{UserID: "U1", EatenAt: time.Now(), Items: []meal.FoodItem{{Name: "拿鐵"}}, Total: meal.Nutrients{Calories: 190}}}}
	h, _ := newFoodHandler(lunchJSON, repo)

	if !h.handleFoodKeyword(context.Background(), "U1", "今天吃什麼", "tok") || !strings.Contains(api.lastReply(t), "拿鐵：190 大卡") {
		t.Errorf("today: %s", api.lastReply(t))
	}
	if !h.handleFoodKeyword(context.Background(), "U1", keywordDeleteMeal, "tok") || !repo.deleted || !strings.Contains(api.lastReply(t), "已刪除") {
		t.Errorf("delete: %s", api.lastReply(t))
	}
	h.handleFoodKeyword(context.Background(), "U1", keywordDeleteMeal, "tok")
	if !strings.Contains(api.lastReply(t), "沒有可以刪除") {
		t.Errorf("delete empty: %s", api.lastReply(t))
	}
	if h.handleFoodKeyword(context.Background(), "U1", "信義區", "tok") {
		t.Error("place names must fall through to weather lookup")
	}
}

func (f *fakeMealRepo) DeleteByID(context.Context, string, string) (bool, error) { return false, nil }
