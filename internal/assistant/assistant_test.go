package assistant

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 台灣 2026-11-02（週一）20:00.
var fixedNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

type fakeCompleter struct {
	answers []string
	reqs    []*ai.Request
}

func (f *fakeCompleter) Complete(_ context.Context, req *ai.Request) ai.Result {
	f.reqs = append(f.reqs, req)
	for _, a := range f.answers {
		if req.Accept == nil || req.Accept(a) {
			return ai.Result{Text: a, Provider: "claude", Model: "claude-opus-5-5"}
		}
	}
	return ai.Result{Errors: []string{"claude(m) 429: rate limited org_secret12345"}}
}

type memHistory struct{ turns map[string][]Turn }

func (m *memHistory) Recent(_ context.Context, uid string) []Turn { return m.turns[uid] }

func (m *memHistory) Append(_ context.Context, uid string, t Turn) {
	m.turns[uid] = append(m.turns[uid], t)
	if len(m.turns[uid]) > historyTurns {
		m.turns[uid] = m.turns[uid][1:]
	}
}

func (m *memHistory) Clear(_ context.Context, uid string) error {
	delete(m.turns, uid)
	return nil
}

type stubMeals struct {
	meals []meal.Meal
	err   error
}

func (s *stubMeals) FindByUserBetween(_ context.Context, uid string, from, to time.Time) ([]meal.Meal, error) {
	if s.err != nil {
		return nil, s.err
	}
	var out []meal.Meal
	for i := range s.meals {
		if m := s.meals[i]; m.UserID == uid && !m.EatenAt.Before(from) && m.EatenAt.Before(to) {
			out = append(out, m)
		}
	}
	return out, nil
}

type stubWorkouts struct{ workouts []workout.Workout }

func (s *stubWorkouts) FindByUserBetween(_ context.Context, uid string, from, to time.Time) ([]workout.Workout, error) {
	var out []workout.Workout
	for i := range s.workouts {
		if w := s.workouts[i]; w.UserID == uid && !w.DoneAt.Before(from) && w.DoneAt.Before(to) {
			out = append(out, w)
		}
	}
	return out, nil
}

type stubProfiles struct {
	p   *profile.Profile
	err error
}

func (s *stubProfiles) Get(context.Context, string) (*profile.Profile, error) { return s.p, s.err }

type stubWeather struct{ line string }

func (s stubWeather) ForUser(context.Context, string) string { return s.line }

func newTestAssistant(c *fakeCompleter, withData bool) (*Assistant, *memHistory) {
	h := &memHistory{turns: map[string][]Turn{}}
	cfg := &Config{
		Completer: c, History: h, Order: []string{"claude", "groq"},
		Meals: &stubMeals{}, Workouts: &stubWorkouts{}, Profiles: &stubProfiles{},
	}
	if withData {
		cfg.Meals = &stubMeals{meals: []meal.Meal{
			{UserID: "U1", EatenAt: fixedNow.Add(-4 * time.Hour), Items: []meal.FoodItem{{Name: "雞腿便當"}}, Total: meal.Nutrients{Calories: 820}},
			{UserID: "U1", EatenAt: fixedNow.Add(-28 * time.Hour), Total: meal.Nutrients{Calories: 1800}},
			{UserID: "U2", EatenAt: fixedNow, Items: []meal.FoodItem{{Name: "別人的晚餐"}}, Total: meal.Nutrients{Calories: 999}},
		}}
		cfg.Workouts = &stubWorkouts{workouts: []workout.Workout{
			{UserID: "U1", Activity: "跑步", DurationMin: 30, DistanceKM: 5, Calories: 343, DoneAt: fixedNow.Add(-1 * time.Hour)},
		}}
		cfg.Profiles = &stubProfiles{p: &profile.Profile{WeightKG: 70, HeightCM: 175, Age: 30, Sex: profile.SexMale}}
		cfg.Weather = stubWeather{line: "使用者最近查詢的地點（台北市信義區）天氣：天氣現象 多雲、12小時降雨機率 20%"}
	}
	a := New(cfg)
	a.now = func() time.Time { return fixedNow }
	return a, h
}

func TestAnswerUsesTheUsersOwnData(t *testing.T) {
	c := &fakeCompleter{answers: []string{"今天還可以吃約 1500 大卡，晚餐建議…"}}
	a, _ := newTestAssistant(c, true)

	got, err := a.Answer(context.Background(), "U1", "今天還能吃多少？")
	if err != nil || got.Text != "今天還可以吃約 1500 大卡，晚餐建議…" || got.MentionsFood {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	req := c.reqs[0]
	if req.System != systemPrompt || strings.Join(req.Order, ",") != "claude,groq" {
		t.Errorf("system/order wrong: order=%v", req.Order)
	}
	for _, want := range []string{
		"現在：2026-11-02（週一）20:00", "體重 70.0 公斤", "基礎代謝＋日常約 1979 大卡",
		"今天：攝取 820 大卡（1 餐：雞腿便當）", "運動消耗 343 大卡（跑步 30 分鐘・5 公里）", "維持體重約需 2322 大卡",
		"近 7 天：有飲食紀錄 2 天、平均每天攝取 1310 大卡", "台北市信義區", "【問題】\n今天還能吃多少？",
	} {
		if !strings.Contains(req.Prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, req.Prompt)
		}
	}
	if strings.Contains(req.Prompt, "別人的晚餐") {
		t.Error("another user's data leaked into the prompt")
	}
	if strings.Index(req.Prompt, "【使用者資料】") > strings.Index(req.Prompt, "【問題】") {
		t.Error("user data must come before the question")
	}
}

func TestAnswerWithoutAnyData(t *testing.T) {
	c := &fakeCompleter{answers: []string{"建議先拍照記錄三餐。"}}
	a, _ := newTestAssistant(c, false)
	if _, err := a.Answer(context.Background(), "U1", "我吃得健康嗎"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.reqs[0].Prompt, "使用者還沒有飲食、運動或身體資料紀錄") {
		t.Errorf("prompt: %s", c.reqs[0].Prompt)
	}
}

func TestFollowUpsIncludeRecentTurns(t *testing.T) {
	c := &fakeCompleter{answers: []string{"可以吃雞胸肉沙拉。"}}
	a, h := newTestAssistant(c, false)
	_, _ = a.Answer(context.Background(), "U1", "晚餐吃什麼？")
	_, _ = a.Answer(context.Background(), "U1", "那熱量多少？")
	if !strings.Contains(c.reqs[1].Prompt, "【先前對話】\n使用者：晚餐吃什麼？\n助理：可以吃雞胸肉沙拉。") {
		t.Errorf("follow-up lacks history:\n%s", c.reqs[1].Prompt)
	}
	if err := a.Forget(context.Background(), "U1"); err != nil {
		t.Fatal(err)
	}
	if len(h.turns["U1"]) != 0 {
		t.Error("Forget must clear the history")
	}

	// [F24-46] Redis 沒設定時（NewRedisHistory(nil)）第二輪也帶有先前對話.
	c2 := &fakeCompleter{answers: []string{"可以吃雞胸肉沙拉。"}}
	a2 := New(&Config{
		Completer: c2, History: NewRedisHistory(nil), Order: []string{"claude", "groq"},
		Meals: &stubMeals{}, Workouts: &stubWorkouts{}, Profiles: &stubProfiles{},
	})
	a2.now = func() time.Time { return fixedNow }
	_, _ = a2.Answer(context.Background(), "U1", "晚餐吃什麼？")
	_, _ = a2.Answer(context.Background(), "U1", "那熱量多少？")
	if !strings.Contains(c2.reqs[1].Prompt, "【先前對話】\n使用者：晚餐吃什麼？\n助理：可以吃雞胸肉沙拉。") {
		t.Errorf("follow-up without Redis lacks history:\n%s", c2.reqs[1].Prompt)
	}
	if err := a2.Forget(context.Background(), "U1"); err != nil {
		t.Fatal(err)
	}
	_, _ = a2.Answer(context.Background(), "U1", "再問一次")
	if strings.Contains(c2.reqs[2].Prompt, "【先前對話】\n使用者：晚餐吃什麼？") {
		t.Error("Forget must clear the local history")
	}
}

// 資料庫讀取失敗不能說成「沒有紀錄」，否則模型會告訴使用者他什麼都沒記.
func TestReadFailuresAreNotReportedAsNoRecords(t *testing.T) {
	dbDown := errors.New("server selection timeout")
	tests := []struct {
		name      string
		meals     *stubMeals
		profiles  *stubProfiles
		want      []string
		forbidden []string
	}{
		{
			name:      "records unreadable",
			meals:     &stubMeals{err: dbDown},
			profiles:  &stubProfiles{},
			want:      []string{"飲食運動紀錄：暫時讀取失敗"},
			forbidden: []string{"還沒有", "沒有飲食或運動紀錄"},
		},
		{
			name:      "profile unreadable, records empty",
			meals:     &stubMeals{},
			profiles:  &stubProfiles{err: dbDown},
			want:      []string{"身體資料：暫時讀取失敗", "近 7 天沒有飲食或運動紀錄"},
			forbidden: []string{"還沒有"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &fakeCompleter{answers: []string{"好的"}}
			a, _ := newTestAssistant(c, false)
			a.meals, a.profiles = tt.meals, tt.profiles
			if _, err := a.Answer(context.Background(), "U1", "我今天吃多少？"); err != nil {
				t.Fatal(err)
			}
			prompt := c.reqs[0].Prompt
			for _, w := range tt.want {
				if !strings.Contains(prompt, w) {
					t.Errorf("prompt lacks %q:\n%s", w, prompt)
				}
			}
			for _, f := range tt.forbidden {
				if strings.Contains(prompt, f) {
					t.Errorf("prompt must not contain %q:\n%s", f, prompt)
				}
			}
		})
	}
}

func TestUnusableAnswersAreRejected(t *testing.T) {
	c := &fakeCompleter{answers: []string{"   ", "\n**  **\n# "}}
	a, h := newTestAssistant(c, false)
	if _, err := a.Answer(context.Background(), "U1", "問題"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if len(h.turns) != 0 {
		t.Error("failed answers must not be stored")
	}
}

func TestCleanAnswerStripsMarkdown(t *testing.T) {
	got := cleanAnswer("## 建議\n**多喝水**，少喝含糖飲料。\n\n# 結論\n加油")
	if got != "建議\n多喝水，少喝含糖飲料。\n\n結論\n加油" {
		t.Errorf("got %q", got)
	}
}

func TestLongAnswerIsTrimmedNotRejected(t *testing.T) {
	c := &fakeCompleter{answers: []string{strings.Repeat("多喝水少熬夜。", 300)}}
	a, h := newTestAssistant(c, false)
	reply, err := a.Answer(context.Background(), "U1", "問題")
	if err != nil {
		t.Fatalf("a long answer must still be used: %v", err)
	}
	got := reply.Text
	if n := len([]rune(got)); n > maxAnswerRunes || !strings.HasSuffix(got, "。") {
		t.Errorf("want ≤%d runes ending at a sentence, got %d runes ending %q", maxAnswerRunes, n, string([]rune(got)[n-1:]))
	}
	if len(h.turns["U1"]) != 1 || h.turns["U1"][0].Answer != got {
		t.Error("history must keep the trimmed answer")
	}
}

func TestTruncateAtSentence(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"short stays", "你好。", "你好。"},
		{"cut at last sentence end", "一二三四五。六七八九", "一二三四五。"},
		{"sentence end too early keeps more text", "一二三。四五六七八九", "一二三。四五六…"},
		{"no sentence end at all", "一二三四五六七八九十", "一二三四五六七…"},
		{"newline counts as an end", "一二三四五\n六七八九十", "一二三四五"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateAtSentence(tt.in, 8); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCleanQuestion(t *testing.T) {
	if got := CleanQuestion("  晚餐\n吃\x00什麼\t？ "); got != "晚餐 吃什麼 ？" {
		t.Errorf("got %q", got)
	}
	if got := CleanQuestion(strings.Repeat("問", 600)); len([]rune(got)) != maxQuestionRunes {
		t.Errorf("length %d", len([]rune(got)))
	}
	c := &fakeCompleter{answers: []string{"x"}}
	a, _ := newTestAssistant(c, false)
	if _, err := a.Answer(context.Background(), "U1", " \n "); !errors.Is(err, ErrUnavailable) || len(c.reqs) != 0 {
		t.Error("empty question must not call AI")
	}
}

func TestNewWithoutCompleter(t *testing.T) {
	if New(&Config{}) != nil {
		t.Error("no completer means AI disabled")
	}
	var a *Assistant
	if err := a.Forget(context.Background(), "U1"); err != nil {
		t.Errorf("nil assistant Forget = %v", err)
	}
}

func TestProfileWithoutRecordsSaysSo(t *testing.T) {
	c := &fakeCompleter{answers: []string{"ok"}}
	a := New(&Config{
		Completer: c, Meals: &stubMeals{}, Workouts: &stubWorkouts{},
		Profiles: &stubProfiles{p: &profile.Profile{WeightKG: 60}},
	})
	a.now = func() time.Time { return fixedNow }
	_, _ = a.Answer(context.Background(), "U1", "q")
	p := c.reqs[0].Prompt
	if !strings.Contains(p, "近 7 天沒有飲食或運動紀錄") || strings.Contains(p, "攝取 0 大卡") {
		t.Errorf("prompt: %s", p)
	}
}

// 只說了一樣食物時模型會加標記；標記要移除，不能出現在回覆或對話紀錄裡.
func TestMealMarkerIsStrippedAndReported(t *testing.T) {
	c := &fakeCompleter{answers: []string{"牛肉麵一碗約 550 到 700 大卡，湯頭偏鹹。\n[[MEAL]]"}}
	a, h := newTestAssistant(c, false)
	got, err := a.Answer(context.Background(), "U1", "牛肉麵")
	if err != nil || !got.MentionsFood || got.Text != "牛肉麵一碗約 550 到 700 大卡，湯頭偏鹹。" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if strings.Contains(h.turns["U1"][0].Answer, "[[") {
		t.Errorf("marker leaked into history: %q", h.turns["U1"][0].Answer)
	}
	if !strings.Contains(systemPrompt, mealMarker) {
		t.Error("the prompt must tell the model about the marker")
	}
}

func TestAnswerThatIsOnlyTheMarkerIsRejected(t *testing.T) {
	c := &fakeCompleter{answers: []string{"[[MEAL]]", " \n[[MEAL]]\n "}}
	a, _ := newTestAssistant(c, false)
	if _, err := a.Answer(context.Background(), "U1", "牛肉麵"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("an answer with nothing but the marker must count as unusable, err=%v", err)
	}
}

func turnsText(turns []Turn) []string {
	out := make([]string, len(turns))
	for i, t := range turns {
		out[i] = t.Question + "/" + t.Answer
	}
	return out
}

// [F24-46] Redis 為 nil 時對話紀錄仍可用：保留最近 3 輪、由舊到新.
func TestHistoryWorksWithoutRedis(t *testing.T) {
	ctx := context.Background()
	h := NewRedisHistory(nil)
	if h == nil {
		t.Fatal("NewRedisHistory(nil) must be non-nil")
	}
	for i := 1; i <= 4; i++ {
		n := string(rune('0' + i))
		h.Append(ctx, "U1", Turn{Question: "q" + n, Answer: "a" + n})
	}
	tests := []struct {
		name   string
		userID string
		want   []string
	}{
		{"latest three oldest first", "U1", []string{"q2/a2", "q3/a3", "q4/a4"}},
		{"other user empty", "U2", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := turnsText(h.Recent(ctx, tt.userID))
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
	if err := h.Clear(ctx, "U1"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got := h.Recent(ctx, "U1"); len(got) != 0 {
		t.Errorf("after Clear: %v", got)
	}
}

// [F24-46] Redis 出錯時走本地；Clear 回 Redis err 但本地已清.
func TestHistoryClearRemovesLocalWhenRedisFails(t *testing.T) {
	ctx := context.Background()
	h := NewRedisHistory(deadRedis(t))
	h.Append(ctx, "U1", Turn{Question: "q1", Answer: "a1"})
	if got := turnsText(h.Recent(ctx, "U1")); strings.Join(got, ",") != "q1/a1" {
		t.Fatalf("local fallback missing: %v", got)
	}
	if err := h.Clear(ctx, "U1"); err == nil {
		t.Error("Clear should return the Redis error")
	}
	if got := h.Recent(ctx, "U1"); len(got) != 0 {
		t.Errorf("local not cleared: %v", got)
	}
}

// historyFlakyHook go-redis Hook：down 為 true 時指令回 error，否則不呼叫 next 直接當作成功（不連網路）.
type historyFlakyHook struct{ down atomic.Bool }

func (h *historyFlakyHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *historyFlakyHook) ProcessHook(_ redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		if h.down.Load() {
			err := errors.New("redis down")
			cmd.SetErr(err)
			return err
		}
		return nil
	}
}

func (h *historyFlakyHook) ProcessPipelineHook(_ redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(_ context.Context, _ []redis.Cmder) error {
		if h.down.Load() {
			return errors.New("redis down")
		}
		return nil
	}
}

// [F24-71] Redis 壞 -> 好 -> 壞：好的那次 Append 成功後本地舊對話要清掉，第二次壞時不得讀回舊值.
func TestHistoryClearsLocalAfterRedisRecovers(t *testing.T) {
	ctx := context.Background()
	hook := &historyFlakyHook{}
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { _ = c.Close() })
	c.AddHook(hook)
	h := NewRedisHistory(c)

	hook.down.Store(true)
	h.Append(ctx, "U1", Turn{Question: "old", Answer: "old"}) // 壞：寫進本地
	if got := turnsText(h.Recent(ctx, "U1")); strings.Join(got, ",") != "old/old" {
		t.Fatalf("local fallback should hold old, got %v", got)
	}
	hook.down.Store(false)
	h.Append(ctx, "U1", Turn{Question: "new", Answer: "new"}) // 好：TxPipeline 成功
	hook.down.Store(true)
	if got := h.Recent(ctx, "U1"); len(got) != 0 {
		t.Errorf("stale local turns must not be read back after Redis recovered, got %v", got)
	}
}

// [F24-46] 並行 Append 無 data race，長度不超過 3.
func TestHistoryConcurrentAppendIsSafe(t *testing.T) {
	ctx := context.Background()
	h := NewRedisHistory(nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Append(ctx, "U1", Turn{Question: "q", Answer: "a"})
			_ = h.Recent(ctx, "U1")
		}()
	}
	wg.Wait()
	if got := h.Recent(ctx, "U1"); len(got) == 0 || len(got) > historyTurns {
		t.Errorf("len = %d, want 1..%d", len(got), historyTurns)
	}
}

// deadRedis 連到一定連不上的埠，所有操作回非 redis.Nil 的 error（repo 沒有 miniredis）.
func deadRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{
		Addr:          "127.0.0.1:1",
		MaxRetries:    -1,
		DialTimeout:   50 * time.Millisecond,
		DialerRetries: 1,
	})
	t.Cleanup(func() { _ = c.Close() })
	return c
}
