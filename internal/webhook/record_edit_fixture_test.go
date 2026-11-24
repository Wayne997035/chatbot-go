package webhook

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/assistant/recordedit"
	"chatbot-go/internal/line"
	"chatbot-go/internal/models"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/workout"
	"chatbot-go/internal/storage/storagetest"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// [F33-22][F33-23] 修改紀錄的測試設備.
// 只有「AI 的模型呼叫」是假的（planAI 回事先排好的計畫 JSON）與 DB 錯誤注入；
// 規劃器（*assistant.Assistant.PlanEdit）、recordedit.Build*Proposal / Apply*、暫存（以 userID 為鍵的記憶體 Store）都是真的實作.

// planAI 排好答案的假模型：每次 Complete 取下一個答案（答案用完回失敗）；記錄呼叫次數與最後一次請求.
type planAI struct {
	mu      sync.Mutex
	answers []string
	calls   int
	last    *ai.Request
}

func (p *planAI) Complete(_ context.Context, req *ai.Request) ai.Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.last = req
	if len(p.answers) == 0 {
		return ai.Result{Errors: []string{"claude(m) 529: overloaded org_secret999999"}}
	}
	answer := p.answers[0]
	p.answers = p.answers[1:]
	if req.Accept != nil && !req.Accept(answer) {
		return ai.Result{Errors: []string{"claude(m) bad answer"}}
	}
	return ai.Result{Text: answer, Provider: "claude", Model: "m"}
}

func (p *planAI) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// countingPlanner 數 PlanEdit 被呼叫幾次；fn 非 nil 時用它的結果（模擬不同的 Planner 實作），否則交給真的規劃器.
type countingPlanner struct {
	inner recordedit.Planner
	fn    func(ctx context.Context, userID, text string) (*recordedit.Outcome, error)
	calls atomic.Int64
}

func (c *countingPlanner) PlanEdit(ctx context.Context, userID, text string) (*recordedit.Outcome, error) {
	c.calls.Add(1)
	if c.fn != nil {
		return c.fn(ctx, userID, text)
	}
	return c.inner.PlanEdit(ctx, userID, text)
}

// editFixture 在記餐設備上加上運動儲存、暫存與規劃器.
type editFixture struct {
	*mealFixture
	workouts *storagetest.Workouts
	store    *storagetest.PendingEdits
	plan     *planAI
	planner  *countingPlanner
	now      time.Time // h.clock 回傳的時間（測試可改）
	mu       sync.Mutex
}

func newEditFixture(t *testing.T, mealTexts ...string) *editFixture {
	t.Helper()
	f := &editFixture{
		mealFixture: newMealFixture(t, mealTexts...),
		workouts:    &storagetest.Workouts{},
		store:       storagetest.NewPendingEdits(),
		plan:        &planAI{},
		now:         time.Now(),
	}
	f.planner = &countingPlanner{inner: assistant.New(&assistant.Config{Completer: f.plan, Meals: f.meals, Workouts: f.workouts})}
	f.h.WithFitness(f.workouts, nil).WithRecordEdit(f.store)
	f.h.editPlanner = f.planner
	f.h.clock = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}
	return f
}

// setNow 改 handler 的時鐘.
func (f *editFixture) setNow(t time.Time) {
	f.mu.Lock()
	f.now = t
	f.mu.Unlock()
}

// seedMeal 新增一筆餐點（單品項）並回傳存起來的版本.
func (f *editFixture) seedMeal(t *testing.T, userID, name string, kcal float64, eatenAt time.Time) *meal.Meal {
	t.Helper()
	m := &meal.Meal{
		UserID: userID, EatenAt: eatenAt, CreateTime: eatenAt, MealType: "lunch", Source: meal.SourceText,
		Items: []meal.FoodItem{{Name: name, Portion: "1份", Nutrients: meal.Nutrients{Calories: kcal, ProteinG: 5, CarbsG: 20, FatG: 3}}},
	}
	m.Recalc()
	if err := f.meals.Insert(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m
}

// seedWorkout 新增使用者 U1 的一筆運動（跑步 20 分、6 公里、314 大卡）.
func (f *editFixture) seedWorkout(t *testing.T, doneAt time.Time) *workout.Workout {
	t.Helper()
	w := &workout.Workout{UserID: "U1", Activity: "跑步", DurationMin: 20, DistanceKM: 6, Calories: 314, Source: workout.SourceText, DoneAt: doneAt, CreateTime: doneAt}
	if err := f.workouts.Insert(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	return w
}

// 計畫 JSON（模型的輸出）.
const (
	planSetItem210 = `{"op":"set_item_kcal","target":"M1","item":1,"kcal":210,"basis":"約 85 公克 × 每 100 公克 245 大卡"}`
	planAddCheese  = `{"op":"add_item","target":"M1","new_item":{"name":"起司","portion":"1片","calories":70,"protein_g":4,"carbs_g":0.5,"fat_g":6},"basis":"客人說裡面有起司"}`
	planDeleteMeal = `{"op":"delete_meal","target":"M1","basis":"客人要刪掉這餐"}`
	planWorkout30  = `{"op":"set_workout","target":"W1","duration_min":30,"basis":"客人說改成 30 分鐘"}`
	planDelWorkout = `{"op":"delete_workout","target":"W1","basis":"客人要刪掉這筆運動"}`
	planNone       = `{"op":"none"}`
	planGarbage    = `這不是 JSON`
)

// say 送一則文字訊息給指定使用者，回傳最後一則回覆的 JSON 字串（沒有新回覆時 fatal）.
func (f *editFixture) sayAs(t *testing.T, userID, text string) string {
	t.Helper()
	before := len(f.api.replies)
	f.h.handleTextMessage(context.Background(), userID, text, "tok")
	if len(f.api.replies) == before {
		t.Fatalf("no reply to %q", text)
	}
	return f.checked(t, text)
}

// quiet 送一則文字訊息，不要求有回覆.
func (f *editFixture) quiet(userID, text string) {
	f.h.handleTextMessage(context.Background(), userID, text, "tok")
}

// postbackEvent 組一個 postback 事件（結構照 LINE 官方 Postback event 範例）.
func postbackEvent(userID, data string, ts time.Time) *models.Event {
	return &models.Event{
		Type: "postback", ReplyToken: "tok", Timestamp: ts.UnixMilli(), Mode: "active",
		Source:   models.Source{Type: "user", UserID: userID},
		Postback: models.Postback{Data: data},
	}
}

// tap 送出 postback；回傳新增的回覆數（0 = 沒有回覆）.
func (f *editFixture) tap(userID, data string) int {
	before := len(f.api.replies)
	f.h.handleEventSafely(context.Background(), postbackEvent(userID, data, time.Now()))
	return len(f.api.replies) - before
}

// tapReply 送出 postback 並回傳那則回覆（沒有回覆時 fatal）.
func (f *editFixture) tapReply(t *testing.T, userID, data string) string {
	t.Helper()
	if f.tap(userID, data) == 0 {
		t.Fatalf("no reply to postback %q", data)
	}
	return f.checked(t, data)
}

var cardDataRe = regexp.MustCompile(`^ce1:(ok|no):[A-Za-z0-9_-]{22}$`)

// quickReplies 取最後一則回覆的 quick reply action.
func (f *editFixture) quickReplies(t *testing.T) []map[string]any {
	t.Helper()
	msgs, _ := f.api.replies[len(f.api.replies)-1]["messages"].([]any)
	var out []map[string]any
	for _, m := range msgs {
		qr, _ := m.(map[string]any)["quickReply"].(map[string]any)
		items, _ := qr["items"].([]any)
		for _, it := range items {
			a, _ := it.(map[string]any)["action"].(map[string]any)
			out = append(out, a)
		}
	}
	return out
}

// buttonData 回覆裡指定 postback 動作（ok / no / pick）的 data；找不到回空字串.
func (f *editFixture) buttonData(t *testing.T, action string) string {
	t.Helper()
	for _, a := range f.quickReplies(t) {
		if d, _ := a["data"].(string); strings.HasPrefix(d, "ce1:"+action+":") {
			return d
		}
	}
	return ""
}

// proposeMeal 讓使用者 U1 提出一個餐點修改並回傳確認鈕的 data.
func (f *editFixture) proposeAndGetOK(t *testing.T, userID, text, planJSON string) string {
	t.Helper()
	f.plan.mu.Lock()
	f.plan.answers = append(f.plan.answers, planJSON)
	f.plan.mu.Unlock()
	f.sayAs(t, userID, text)
	d := f.buttonData(t, "ok")
	if d == "" {
		t.Fatalf("no confirm button in %s", f.api.lastReply(t))
	}
	return d
}

// lockedLine 並行安全的假 LINE 回覆端點（lineAPI 的 replies 不是並行安全的）.
type lockedLine struct {
	mu      sync.Mutex
	replies []string
	client  *line.Client
}

func newLockedLine(t *testing.T) *lockedLine {
	t.Helper()
	l := &lockedLine{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/reply" {
			data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			l.mu.Lock()
			l.replies = append(l.replies, string(data))
			l.mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	t.Cleanup(srv.Close)
	l.client = line.NewClient(&line.Config{Token: "t", ReplyURL: srv.URL + "/reply", LoadingURL: srv.URL + "/loading", ContentURL: srv.URL + "/content"}, nil)
	return l
}

func (l *lockedLine) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.replies...)
}
