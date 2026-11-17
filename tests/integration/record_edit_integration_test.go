package integration

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/line"
	"chatbot-go/internal/models"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/pendingedit"
	userstore "chatbot-go/internal/storage/database/user"
	weatherstore "chatbot-go/internal/storage/database/weather"
	"chatbot-go/internal/storage/database/workout"
	"chatbot-go/internal/weather"
	"chatbot-go/internal/webhook"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// editLine 並行安全的假 LINE 回覆端點.
type editLine struct {
	mu      sync.Mutex
	replies []string
}

func (l *editLine) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.replies...)
}

// planAnswers 排好答案的假模型（規劃器的模型呼叫）.
type planAnswers struct {
	mu      sync.Mutex
	answers []string
}

func (p *planAnswers) Complete(_ context.Context, req *ai.Request) ai.Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.answers) == 0 {
		return ai.Result{Errors: []string{"no answer"}}
	}
	a := p.answers[0]
	p.answers = p.answers[1:]
	if req.Accept != nil && !req.Accept(a) {
		return ai.Result{Errors: []string{"bad answer"}}
	}
	return ai.Result{Text: a, Provider: "claude", Model: "m"}
}

// editRig 沒有 Redis：餐點、運動、待確認暫存全是真的 MongoDB（testcontainers），規劃器是真的、只有模型呼叫是假的.
type editRig struct {
	h     *webhook.WebhookHandler
	line  *editLine
	plan  *planAnswers
	meals *meal.MealRepo
	works *workout.WorkoutRepo
}

func newEditRig(t *testing.T) *editRig {
	t.Helper()
	db := getTestDB(t)
	r := &editRig{line: &editLine{}, plan: &planAnswers{}, meals: meal.NewMealRepository(db), works: workout.NewWorkoutRepository(db)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/reply" {
			data, _ := io.ReadAll(io.LimitReader(req.Body, 1<<20))
			r.line.mu.Lock()
			r.line.replies = append(r.line.replies, string(data))
			r.line.mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	t.Cleanup(srv.Close)
	lc := line.NewClient(&line.Config{Token: "t", ReplyURL: srv.URL + "/reply", LoadingURL: srv.URL + "/loading", ContentURL: srv.URL + "/content"}, nil)
	planner := assistant.New(&assistant.Config{Completer: r.plan, Meals: r.meals, Workouts: r.works})
	// redisClient 為 nil → Select 回 MongoDB 版（沒有 Redis 的路徑）.
	store := pendingedit.Select(nil, pendingedit.NewMongoStore(db))
	if pendingedit.BackendOf(store) != pendingedit.BackendMongo {
		t.Fatalf("backend = %q, want mongo", pendingedit.BackendOf(store))
	}
	r.h = webhook.NewWebhookHandler(lc, userstore.NewUserRepository(db), weather.NewLookup(weatherstore.NewWeatherRepository(db), nil, 60), nil, conversation.NewMemoryManager()).
		WithNutrition(nil, r.meals, nil).
		WithFitness(r.works, nil, nil).
		WithAssistant(planner, nil).
		WithRecordEdit(store)
	return r
}

// post 把事件當成 LINE webhook 送進 HandleWebhook（非同步處理）.
func (r *editRig) post(t *testing.T, ev *models.Event) {
	t.Helper()
	body, err := json.Marshal(models.EventWrapper{Destination: "x", Events: []models.Event{*ev}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/webhooks", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if err := r.h.HandleWebhook(echo.New().NewContext(req, rec)); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("HandleWebhook: code=%d err=%v", rec.Code, err)
	}
}

func (r *editRig) say(t *testing.T, userID, text string) {
	t.Helper()
	r.post(t, &models.Event{
		Type: "message", ReplyToken: "tok", Timestamp: time.Now().UnixMilli(),
		Source: models.Source{Type: "user", UserID: userID}, Message: models.Message{ID: "1", Type: "text", Text: text},
	})
}

func (r *editRig) tap(t *testing.T, userID, data string) {
	t.Helper()
	r.post(t, &models.Event{
		Type: "postback", ReplyToken: "tok", Timestamp: time.Now().UnixMilli(),
		Source: models.Source{Type: "user", UserID: userID}, Postback: models.Postback{Data: data},
	})
}

// waitReplies 等到收到至少 n 則回覆（事件是背景處理的）.
func (r *editRig) waitReplies(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if got := r.line.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d replies, have %d: %v", n, len(r.line.snapshot()), r.line.snapshot())
	return nil
}

// okData 從最後一則回覆取出 ok 按鈕的 data.
func okData(t *testing.T, reply string) string {
	t.Helper()
	var payload struct {
		Messages []struct {
			QuickReply struct {
				Items []struct {
					Action struct {
						Data string `json:"data"`
					} `json:"action"`
				} `json:"items"`
			} `json:"quickReply"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(reply), &payload); err != nil {
		t.Fatal(err)
	}
	for _, m := range payload.Messages {
		for _, it := range m.QuickReply.Items {
			if strings.HasPrefix(it.Action.Data, "ce1:ok:") {
				return it.Action.Data
			}
		}
	}
	t.Fatalf("no confirm button in %s", reply)
	return ""
}

func (r *editRig) seedMeal(t *testing.T, userID, name string, kcal float64) *meal.Meal {
	t.Helper()
	m := &meal.Meal{
		UserID: userID, EatenAt: time.Now().Add(-time.Minute), CreateTime: time.Now().Add(-time.Minute), MealType: "lunch", Source: meal.SourceText,
		Items: []meal.FoodItem{{Name: name, Portion: "1份", Nutrients: meal.Nutrients{Calories: kcal}}},
	}
	m.Recalc()
	if err := r.meals.Insert(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m
}

const (
	itPlanSet210   = `{"op":"set_item_kcal","target":"M1","item":1,"kcal":210,"basis":"約 85 公克 × 每 100 公克 245 大卡"}`
	itPlanAddCheez = `{"op":"add_item","target":"M1","new_item":{"name":"起司","portion":"1片","calories":70,"protein_g":4,"carbs_g":0.5,"fat_g":6}}`
)

// [F33-21] A44：沒有 Redis 的整條路徑（webhook + MongoDB 暫存 + 真的 MongoDB 餐點）：提議 → 確認寫入、單次使用、
// 跨使用者取不到、雙擊併發在 Mongo 後端也恰好一次.
func TestEditFlowOnMongoBackend(t *testing.T) {
	r := newEditRig(t)
	ctx := context.Background()
	m1 := r.seedMeal(t, "U1", "好市多黑麥酸種麵包", 120)
	m2 := r.seedMeal(t, "U2", "別人的飯糰", 300)

	// 提議：不寫入.
	r.plan.answers = []string{itPlanSet210}
	r.say(t, "U1", "你要直接更新正確啦")
	replies := r.waitReplies(t, 1)
	ok := okData(t, replies[len(replies)-1])
	if got, _ := r.meals.FindByID(ctx, "U1", m1.ID); got.Total.Calories != 120 || got.Version != 0 {
		t.Fatalf("written before the confirmation: %+v", got)
	}

	// U2 拿 U1 的 nonce 按確認 → 取不到，U1 的紀錄與提議都不動.
	r.tap(t, "U2", ok)
	replies = r.waitReplies(t, 2)
	if !strings.Contains(replies[1], "過期") {
		t.Errorf("cross-user tap reply: %s", replies[1])
	}
	if got, _ := r.meals.FindByID(ctx, "U1", m1.ID); got.Total.Calories != 120 {
		t.Fatalf("another user's tap changed the record: %+v", got)
	}
	if got, _ := r.meals.FindByID(ctx, "U2", m2.ID); got.Total.Calories != 300 {
		t.Fatalf("U2's own record changed: %+v", got)
	}

	// U1 確認 → 寫入 MongoDB.
	r.tap(t, "U1", ok)
	replies = r.waitReplies(t, 3)
	if !strings.Contains(replies[2], "已改成 210") {
		t.Errorf("confirm reply: %s", replies[2])
	}
	if got, _ := r.meals.FindByID(ctx, "U1", m1.ID); got.Total.Calories != 210 || got.Version != 1 {
		t.Fatalf("stored meal = %+v, want 210 / version 1", got)
	}

	// 單次使用：再按一次 → 過期，資料不再變.
	r.tap(t, "U1", ok)
	replies = r.waitReplies(t, 4)
	if !strings.Contains(replies[3], "過期") {
		t.Errorf("second tap reply: %s", replies[3])
	}
	if got, _ := r.meals.FindByID(ctx, "U1", m1.ID); got.Version != 1 {
		t.Errorf("applied twice: %+v", got)
	}

	// 雙擊併發：加料只加一次.
	r.plan.answers = []string{itPlanAddCheez}
	r.say(t, "U1", "這裡面有起司，請重新分析")
	replies = r.waitReplies(t, 5)
	ok = okData(t, replies[4])
	const taps = 8
	for range taps {
		r.tap(t, "U1", ok) // HandleWebhook 立刻回 200、事件在背景並行處理
	}
	replies = r.waitReplies(t, 5+taps)
	var applied, expired int
	for _, rep := range replies[5:] {
		switch {
		case strings.Contains(rep, "已改成"):
			applied++
		case strings.Contains(rep, "過期"):
			expired++
		}
	}
	if applied != 1 || expired != taps-1 {
		t.Errorf("applied=%d expired=%d, want 1/%d", applied, expired, taps-1)
	}
	got, _ := r.meals.FindByID(ctx, "U1", m1.ID)
	if len(got.Items) != 2 || got.Total.Calories != 280 {
		t.Errorf("stored meal after the double tap = %d items, %v kcal; want 2 items, 280 kcal (210+70)", len(got.Items), got.Total.Calories)
	}
}

// [F33-20] 加 version 欄位之前存的運動沒有這個欄位，也要能用聊天修改（Version 視為 0）.
func TestWorkoutRepo_LegacyWorkoutWithoutVersionCanBeUpdated(t *testing.T) {
	db := getTestDB(t)
	repo := workout.NewWorkoutRepository(db)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	legacy := bson.M{"_id": "legacyw1", "userId": "U1", "activity": "跑步", "durationMin": 20.0, "distanceKm": 6.0, "calories": 314.0, "source": "text", "doneAt": now, "createTime": now}
	if _, err := db.Collection("workout").InsertOne(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	w, err := repo.FindByID(ctx, "U1", "legacyw1")
	if err != nil || w == nil || w.Version != 0 {
		t.Fatalf("find legacy: %+v %v", w, err)
	}
	w.DurationMin = 30
	if ok, err := repo.Update(ctx, w); !ok || err != nil || w.Version != 1 {
		t.Fatalf("update legacy: ok=%v err=%v version=%d", ok, err, w.Version)
	}
	got, _ := repo.FindByID(ctx, "U1", "legacyw1")
	if got.DurationMin != 30 || got.Version != 1 || got.DistanceKM != 6 || !got.DoneAt.Equal(now) {
		t.Errorf("stored = %+v", got)
	}
}
