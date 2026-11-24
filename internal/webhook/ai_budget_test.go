package webhook

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/intent"
	"chatbot-go/internal/nutrition"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// [F36-01] 每則訊息 AI 預算的 webhook 層測試.
// 用「真」ai.Router + 假 Provider 接進 intent.New、assistant.New、nutrition.NewAnalyzer：
// 只用各套件的假 Completer 會繞過 Router，預算就測不到.

// budgetProvider 依請求種類回腳本答案的假供應商；一個模型，所以一次 Router.Complete = 一次 Call.
type budgetProvider struct {
	mu      sync.Mutex
	plans   []string // 規劃器依序的答案；用完回失敗
	qa      []string // 問答依序的答案（"" = 失敗）；用完回最後一個
	calls   map[string]int
	total   int
	qaCalls int
}

func newBudgetProvider() *budgetProvider { return &budgetProvider{calls: map[string]int{}} }

func (p *budgetProvider) Name() string { return "groq" }

func (p *budgetProvider) Candidates(context.Context, ai.Capability) ([]string, error) {
	return []string{"m1"}, nil
}

func (p *budgetProvider) Call(_ context.Context, _ string, req *ai.Request) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total++
	switch {
	case req.Final:
		p.calls["qa"]++
		i := p.qaCalls
		p.qaCalls++
		if len(p.qa) == 0 {
			return "", errors.New("qa failed")
		}
		if i >= len(p.qa) {
			i = len(p.qa) - 1
		}
		if p.qa[i] == "" {
			return "", errors.New("qa failed")
		}
		return p.qa[i], nil
	case strings.Contains(req.System, "修改紀錄規劃器"):
		p.calls["plan"]++
		if len(p.plans) == 0 {
			return "", errors.New("plan failed")
		}
		a := p.plans[0]
		p.plans = p.plans[1:]
		return a, nil
	case strings.Contains(req.System, "意圖分類器"):
		p.calls["intent"]++
		return `{"intent":"none","place":"","confidence":"high"}`, nil
	case strings.Contains(req.Prompt, "【使用者描述】"):
		p.calls["meal"]++
		return "", errors.New("meal estimate must not run in these tests")
	}
	p.calls["other"]++
	return "", errors.New("unexpected request")
}

func (p *budgetProvider) count(kind string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[kind]
}

func (p *budgetProvider) totalCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total
}

const (
	budgetQAAnswer = "晚餐可以選擇清淡一點的蛋白質搭配蔬菜。"
	// J2 最壞訊息原文（規格 User journeys）.
	worstCaseText = "把午餐的麵包改成 210 大卡，晚餐吃了牛肉麵"
	// 規劃器回 op=none 卻列了兩個候選：會觸發一次重問.
	planNoneWithCandidates = `{"op":"none","candidates":["M1","M2"]}`
)

// newBudgetFixture 真 Router + 假 Provider，接進規劃器 / 問答（同一個 Assistant）、意圖、記餐.
func newBudgetFixture(t *testing.T, p *budgetProvider) *editFixture {
	t.Helper()
	f, _ := newBudgetFixtureCounting(t, p)
	return f
}

// assistantCompleteCounter 數問答（Assistant）端呼叫 Router.Complete 的次數，含被 Router 因額度用完而跳過的.
// [F36-01] 只數供應商 Call 看不出「問答重試有沒有先問 CanSpend」：重試就算被 Router 擋下，也不會到供應商.
type assistantCompleteCounter struct {
	inner *ai.Router
	mu    sync.Mutex
	n     int
}

func (c *assistantCompleteCounter) Complete(ctx context.Context, req *ai.Request) ai.Result {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.inner.Complete(ctx, req)
}

func (c *assistantCompleteCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// newBudgetFixtureCounting 同 newBudgetFixture，另回傳數問答端 Complete 次數的計數器.
func newBudgetFixtureCounting(t *testing.T, p *budgetProvider) (*editFixture, *assistantCompleteCounter) {
	t.Helper()
	f := newEditFixture(t)
	router := ai.NewRouter(&ai.Settings{TextOrder: []string{"groq"}}, p)
	counter := &assistantCompleteCounter{inner: router}
	a := assistant.New(&assistant.Config{Completer: counter, Meals: f.meals, Workouts: f.workouts})
	f.h.WithAssistant(a, nil).
		WithNutrition(nutrition.NewAnalyzer(router), f.meals, nil).
		WithIntent(intent.New(router))
	now := f.now
	f.seedMeal(t, "U1", "麵包", 250, now.Add(-2*time.Hour))
	f.seedMeal(t, "U1", "沙拉", 120, now.Add(-1*time.Hour))
	return f, counter
}

// lastReplyTexts 最後一則回覆裡所有 text 訊息（整句比對用）.
func lastReplyTexts(t *testing.T, f *editFixture) []string {
	t.Helper()
	if len(f.api.replies) == 0 {
		t.Fatal("no reply sent")
	}
	msgs, _ := f.api.replies[len(f.api.replies)-1]["messages"].([]any)
	var out []string
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			if s, ok := mm["text"].(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// dailyUntouched 該種每日計數還沒被扣過：用上限 1 去 Incr，沒人扣過才回 true.
func dailyUntouched(f *editFixture, kind string) bool {
	return f.h.withinDailyLimit(context.Background(), kind, "U1", 1)
}

// B3：J2 最壞訊息，規劃器 2 次 + 問答 1 次 = 3；記餐估算 0、意圖 0；每日計數不動.
func TestWorstCaseTextUsesAtMostThreeCompletes(t *testing.T) {
	p := newBudgetProvider()
	p.plans = []string{planNoneWithCandidates, planNoneWithCandidates}
	p.qa = []string{budgetQAAnswer}
	f := newBudgetFixture(t, p)

	f.sayAs(t, "U1", worstCaseText)

	if p.totalCalls() != 3 || p.count("plan") != 2 || p.count("qa") != 1 {
		t.Errorf("total=%d plan=%d qa=%d, want 3/2/1", p.totalCalls(), p.count("plan"), p.count("qa"))
	}
	if p.count("meal") != 0 || p.count("intent") != 0 {
		t.Errorf("meal=%d intent=%d, want 0/0 (skipped by the budget)", p.count("meal"), p.count("intent"))
	}
	if !dailyUntouched(f, "mealtext") || !dailyUntouched(f, "intent:count") {
		t.Error("a skipped step must not burn its daily counter")
	}
}

// B4：規劃器 op=none+2 候選、第二次回有效計畫：重問照做（用 2 次，在上限內）並出確認卡.
func TestPlannerRetryStillAllowedWithinBudget(t *testing.T) {
	p := newBudgetProvider()
	p.plans = []string{planNoneWithCandidates, planSetItem210}
	f := newBudgetFixture(t, p)

	reply := f.sayAs(t, "U1", "把午餐的麵包改成 210 大卡")

	if p.count("plan") != 2 || p.count("qa") != 0 {
		t.Errorf("plan=%d qa=%d, want 2/0", p.count("plan"), p.count("qa"))
	}
	mustContain(t, reply, "210")
}

// B5：問答首輪失敗、前面沒用額度：重試發生（問答共 2 次）並回重試答案.
func TestQARetryRunsWhenBudgetLeft(t *testing.T) {
	p := newBudgetProvider()
	p.qa = []string{"", budgetQAAnswer}
	f := newBudgetFixture(t, p)

	f.sayAs(t, "U1", "你覺得人生的意義是什麼呢")

	if p.count("qa") != 2 || p.totalCalls() != 2 {
		t.Errorf("qa=%d total=%d, want 2/2", p.count("qa"), p.totalCalls())
	}
	if got := strings.Join(lastReplyTexts(t, f), "\n"); !strings.Contains(got, "清淡一點的蛋白質") {
		t.Errorf("reply should carry the retried answer, got %q", got)
	}
}

// B6：前面用 2 次、問答首輪失敗 → 不重試（總 3），回固定句.
func TestQANoRetryWhenPreStepsUsedTwo(t *testing.T) {
	p := newBudgetProvider()
	p.plans = []string{planNoneWithCandidates, planNoneWithCandidates}
	p.qa = []string{"", budgetQAAnswer} // 第二個答案存在，但不該被用到
	f, counter := newBudgetFixtureCounting(t, p)

	f.sayAs(t, "U1", worstCaseText)

	if p.count("qa") != 1 || p.totalCalls() != 3 {
		t.Errorf("qa=%d total=%d, want 1/3 (no retry when the budget is spent)", p.count("qa"), p.totalCalls())
	}
	// 規劃器 2 + 問答 1 = 3 次進 Router.Complete；問答重試要先問 CanSpend，不能丟給 Router 去擋（Router 擋下的呼叫到不了供應商，上面的計數看不出）.
	if counter.count() != 3 {
		t.Errorf("assistant-side Complete calls = %d, want 3 (the QA retry must be skipped by CanSpend, not by Router)", counter.count())
	}
}

// B6：問答失敗回固定句（整句全等；不外洩錯誤）.
func TestQAFailureRepliesFixedSentence(t *testing.T) {
	p := newBudgetProvider()
	p.plans = []string{planNoneWithCandidates, planNoneWithCandidates}
	f := newBudgetFixture(t, p) // p.qa 空 = 問答一律失敗

	f.sayAs(t, "U1", worstCaseText)

	texts := lastReplyTexts(t, f)
	if len(texts) != 1 || texts[0] != "這則我沒看懂，可以換個說法嗎？" {
		t.Errorf("reply texts = %q, want exactly the fixed sentence", texts)
	}
}

// B7：規劃器用 2 次後訊息含「吃」：記餐、意圖靜默跳過，每日計數不增，LINE 沒有「目前無法估算」.
func TestSkippedStepsDoNotBurnDailyCountersOrReply(t *testing.T) {
	p := newBudgetProvider()
	p.plans = []string{planNoneWithCandidates, planNoneWithCandidates}
	p.qa = []string{budgetQAAnswer}
	f := newBudgetFixture(t, p)

	f.sayAs(t, "U1", worstCaseText)

	for _, reply := range f.api.replies {
		if strings.Contains(strings.Join(replyTextsOf(reply), "\n"), "目前無法估算") {
			t.Errorf("a budget-skipped meal step must not reply with an error: %v", reply)
		}
	}
	if !dailyUntouched(f, "mealtext") || !dailyUntouched(f, "intent:count") {
		t.Error("mealtext / intent:count were charged although the steps were skipped")
	}
	if p.count("meal") != 0 || p.count("intent") != 0 {
		t.Errorf("meal=%d intent=%d, want 0/0", p.count("meal"), p.count("intent"))
	}
}

func replyTextsOf(reply map[string]any) []string {
	msgs, _ := reply["messages"].([]any)
	var out []string
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			if s, ok := mm["text"].(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}
