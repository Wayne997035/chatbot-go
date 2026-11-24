package webhook

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/nutrition"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// [F37-41] 記餐文字估算快取的 webhook 層測試：真 ai.Router + 計數供應商，快取在 EstimateText 內.

// mealCacheProvider 記餐估算的計數供應商：每次呼叫都回 answer.
type mealCacheProvider struct {
	mu     sync.Mutex
	answer string
	calls  int
}

func (p *mealCacheProvider) Name() string { return "groq" }

func (p *mealCacheProvider) Candidates(context.Context, ai.Capability) ([]string, error) {
	return []string{"m1"}, nil
}

func (p *mealCacheProvider) Call(_ context.Context, _ string, req *ai.Request) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !strings.Contains(req.Prompt, "【使用者描述】") {
		return "", errors.New("unexpected request")
	}
	p.calls++
	return p.answer, nil
}

func (p *mealCacheProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

const mealCacheNoodle = `{"is_food":true,"meals":[{"meal_type":"","day_offset":0,"snack_only":false,` +
	`"foods":[{"name":"牛肉麵","portion":"1碗","calories":650,"protein_g":32,"carbs_g":80,"fat_g":20}],"store":null,"advice":"湯少喝一點"}]}`

// newMealCacheFixture 真 Router + 計數供應商 + 估算快取（無 Redis）.
func newMealCacheFixture(t *testing.T, answer string) (*mealFixture, *mealCacheProvider) {
	t.Helper()
	f := newMealFixture(t)
	p := &mealCacheProvider{answer: answer}
	router := ai.NewRouter(&ai.Settings{TextOrder: []string{"groq"}}, p)
	analyzer := nutrition.NewAnalyzer(router).WithEstimateCache(nutrition.NewEstimateCache(nil))
	f.h.WithNutrition(analyzer, f.meals, nil)
	return f, p
}

func TestMealTextCacheSharedAcrossUsers(t *testing.T) {
	f, p := newMealCacheFixture(t, mealCacheNoodle)
	const text = "吃了SENTINELMEAL牛肉麵"

	f.h.handleTextMessage(context.Background(), "U-sentinel-1", text, "tok")
	first := f.api.lastReply(t)
	f.h.handleTextMessage(context.Background(), "U-sentinel-2", text, "tok")
	second := f.api.lastReply(t)

	if p.count() != 1 {
		t.Fatalf("[F37-41] provider calls = %d, want 1 (second user must hit the cache)", p.count())
	}
	if len(f.meals.Items) != 2 || f.meals.Items[0].UserID != "U-sentinel-1" || f.meals.Items[1].UserID != "U-sentinel-2" {
		t.Fatalf("each user must have their own record: %+v", f.meals.Items)
	}
	if f.meals.Items[0].Total != f.meals.Items[1].Total || f.meals.Items[1].Total.Calories != 650 {
		t.Errorf("same estimate expected: %+v vs %+v", f.meals.Items[0].Total, f.meals.Items[1].Total)
	}
	mustContain(t, first, "牛肉麵 1碗：650 大卡")
	mustContain(t, second, "牛肉麵 1碗：650 大卡", "建議：湯少喝一點")
}

func TestMealTextCacheHitDoesNotSpendBudget(t *testing.T) {
	f, p := newMealCacheFixture(t, mealCacheNoodle)
	ctx := ai.EnsureBudget(context.Background())

	for _, user := range []string{"U1", "U2", "U3", "U4"} {
		f.h.handleTextMessage(ctx, user, "吃了牛肉麵", "tok")
	}

	if p.count() != 1 {
		t.Fatalf("[F37-41] provider calls = %d, want 1 miss then 3 hits", p.count())
	}
	if len(f.meals.Items) != 4 {
		t.Fatalf("meals = %d, want 4", len(f.meals.Items))
	}
	// 額度只被那 1 次未命中扣過：非 final 的上限是 2 次，所以還能再扣恰好 1 次.
	steps := []struct {
		final bool
		want  bool
	}{{false, true}, {false, false}, {true, true}, {true, false}}
	for i, s := range steps {
		if got := ai.Take(ctx, s.final); got != s.want {
			t.Fatalf("[F37-41] step %d Take(final=%v) = %v, want %v (hits must not spend the budget)", i, s.final, got, s.want)
		}
	}
}

func TestMealTextCacheHitUsesCurrentClock(t *testing.T) {
	f, p := newMealCacheFixture(t, mealCacheNoodle)
	taipei := nutrition.Taipei
	days := time.Date(2026, 11, 24, 0, 0, 0, 0, taipei)

	f.h.clock = func() time.Time { return days.Add(9 * time.Hour) }
	f.h.handleTextMessage(context.Background(), "U1", "吃了牛肉麵", "tok")
	f.h.clock = func() time.Time { return days.Add(21*time.Hour + 30*time.Minute) }
	f.h.handleTextMessage(context.Background(), "U2", "吃了牛肉麵", "tok")

	if p.count() != 1 || len(f.meals.Items) != 2 {
		t.Fatalf("calls=%d meals=%d, want 1 and 2", p.count(), len(f.meals.Items))
	}
	morning, night := f.meals.Items[0], f.meals.Items[1]
	if morning.MealType != string(nutrition.Breakfast) || night.MealType != string(nutrition.LateNight) {
		t.Fatalf("[F37-41] meal types = %q / %q, want breakfast / late_night (a cached value must not pin the first clock)",
			morning.MealType, night.MealType)
	}
	if morning.EatenAt.Hour() != 9 || night.EatenAt.In(taipei).Hour() != 21 {
		t.Errorf("EatenAt must follow the clock at save time: %s / %s", morning.EatenAt, night.EatenAt)
	}
}

func TestMealTextCacheHitCountsDailyLimitAndResend(t *testing.T) {
	f, p := newMealCacheFixture(t, mealCacheNoodle)
	ctx := context.Background()
	f.h.handleTextMessage(ctx, "U1", "吃了牛肉麵", "tok") // miss
	f.h.handleTextMessage(ctx, "U2", "吃了牛肉麵", "tok") // hit
	if p.count() != 1 {
		t.Fatalf("calls = %d, want 1", p.count())
	}
	key := "mealtext:U2:" + f.h.dailyNow().In(nutrition.Taipei).Format("20060102")
	// Incr 回傳加一之後的值：命中那次已算 1，所以這次探測是 2.
	if got := f.h.daily.Incr(key, time.Hour); got != 2 {
		t.Fatalf("[F37-41] daily mealtext count after a hit = %d (probe included), want 2", got)
	}

	// 10 分鐘內同一人重送：仍回「剛剛已經記過這則了」，沒有到估算、沒有新紀錄、每日計數不動.
	before := len(f.meals.Items)
	f.h.handleTextMessage(ctx, "U2", "吃了牛肉麵", "tok")
	mustContain(t, f.api.lastReply(t), "剛剛已經記過這則了")
	if p.count() != 1 || len(f.meals.Items) != before {
		t.Fatalf("resend must not reach the estimate or add a record: calls=%d meals=%d", p.count(), len(f.meals.Items))
	}
	if got := f.h.daily.Incr(key, time.Hour); got != 3 {
		t.Fatalf("resend must not count toward the daily limit: probe = %d, want 3", got)
	}
}
