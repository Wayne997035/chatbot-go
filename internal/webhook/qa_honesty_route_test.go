package webhook

import (
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/nutrition"
	"context"
	"strings"
	"testing"
	"time"
)

// [F34-04][F34-07] 問答誠實：EditTried 結構訊號與「要／好」肯定短句接修改的路由測試.
// 假的只有模型（planAI 排好計畫 JSON、qaCompleter 是問答模型）；規劃器之外的路由、history、暫存都是真的.

const (
	honestyOffer = "我估算約 210 大卡，要不要幫你把這一筆改成這個數字？"
	honestyChat  = "牛肉麵大約 550 大卡。"
)

// honestyFixture 修改紀錄設備 + 真的 *assistant.Assistant（記憶體 history）當問答；planner 仍是 countingPlanner.
type honestyFixture struct {
	*editFixture
	qa   *qaCompleter
	hist assistant.History
}

func newHonestyFixture(t *testing.T) *honestyFixture {
	t.Helper()
	f := &honestyFixture{editFixture: newEditFixture(t), qa: &qaCompleter{answer: "好的，我們聊聊。"}, hist: assistant.NewRedisHistory(nil)}
	f.seedMeal(t, "U1", "麵包", 150, time.Now().Add(-time.Minute))
	f.h.assistant = assistant.New(&assistant.Config{Completer: f.qa, History: f.hist})
	return f
}

// lastAnswer 讓 U1 的 history 最後一輪助理回答是 answer.
func (f *honestyFixture) lastAnswer(answer string) {
	f.hist.Append(context.Background(), "U1", assistant.Turn{Question: "這個多少大卡", Answer: answer})
}

// TestStepAssistantPassesEditTried [F34-04] 規格 #19：規劃器試過沒出卡，落到問答時帶【系統備註】.
func TestStepAssistantPassesEditTried(t *testing.T) {
	f := newHonestyFixture(t)
	f.plan.answers = []string{planNone}
	f.sayAs(t, "U1", "把牛肉三明治改為早餐")
	if got := f.planner.calls.Load(); got != 1 {
		t.Fatalf("planner calls = %d, want 1", got)
	}
	if len(f.qa.asked) != 1 || !strings.Contains(f.qa.asked[0], "【系統備註】") || !strings.Contains(f.qa.asked[0], "沒有產生修改確認卡") {
		t.Fatalf("QA prompt must carry the system note after the planner tried: %q", f.qa.asked)
	}
	// 對照：沒叫過規劃器的訊息（不含修改語氣）不帶備註.
	f.sayAs(t, "U1", "晚餐吃什麼比較好")
	if len(f.qa.asked) != 2 || strings.Contains(f.qa.asked[1], "【系統備註】") {
		t.Errorf("a message the planner never saw must not carry the note: %q", f.qa.asked[len(f.qa.asked)-1])
	}
}

// TestAskPrefixPassesEditTriedFalse [F34-04] 規格 #19：「問 …」路徑沒叫過規劃器，不帶備註.
func TestAskPrefixPassesEditTriedFalse(t *testing.T) {
	f := newHonestyFixture(t)
	f.sayAs(t, "U1", "問 把牛肉三明治改為早餐")
	if got := f.planner.calls.Load(); got != 0 {
		t.Errorf("planner calls = %d, want 0 on the ask-prefix path", got)
	}
	if len(f.qa.asked) != 1 || strings.Contains(f.qa.asked[0], "【系統備註】") {
		t.Errorf("ask-prefix path must not carry the system note: %q", f.qa.asked)
	}
}

// TestTypedConfirmFallsToQAWithoutLie [F34-02] 規格旅程 3：修改功能停用時客人打「確認」落問答，模型回謊話也不會送出去.
// 功能停用時落問答是 E-B 規格寫明的行為（b-regroup-edit.md Risk flags），E-B 合併後仍應為綠.
func TestTypedConfirmFallsToQAWithoutLie(t *testing.T) {
	f := newHonestyFixture(t)
	f.h.WithRecordEdit(nil)
	if f.h.recordEditEnabled() {
		t.Fatal("setup: record edit must be disabled")
	}
	f.qa.answer = "收到，系統已送出修改確認。"
	reply := f.sayAs(t, "U1", "確認")
	if strings.Contains(reply, "系統已送出修改確認") || strings.Contains(reply, "已送出") {
		t.Errorf("the customer must never be told a confirmation was sent: %s", reply)
	}
	mustContain(t, reply, "沒有改到任何紀錄")
}

// TestAffirmativeWordsTable [F34-07] 詞表全等比對：每個詞與帶標點空白的變體為 true，含這些字的長句為 false.
func TestAffirmativeWordsTable(t *testing.T) {
	for _, w := range []string{"要", "好", "好的", "好啊", "可以", "對", "嗯", "OK", "ok", "要！", " 要 ", "好～", "可以。", "好的!", "ok~"} {
		if !isAffirmative(w) {
			t.Errorf("isAffirmative(%q) = false, want true", w)
		}
	}
	for _, w := range []string{"要不要", "好像", "可以嗎", "對啊我午餐吃了牛肉麵", "", "  ", "不要", "不好", "Ok啊", "要要"} {
		if isAffirmative(w) {
			t.Errorf("isAffirmative(%q) = true, want false", w)
		}
	}
}

// TestAffirmativeAfterOfferCallsPlanner [F34-07] 助理剛提議改數字，客人回「要」→ 規劃器 1 次、出確認卡、資料庫不變.
func TestAffirmativeAfterOfferCallsPlanner(t *testing.T) {
	f := newHonestyFixture(t)
	f.lastAnswer(honestyOffer)
	f.plan.answers = []string{planSetItem210}
	f.sayAs(t, "U1", "要")
	if got := f.planner.calls.Load(); got != 1 {
		t.Fatalf("planner calls = %d, want 1", got)
	}
	if f.buttonData(t, "ok") == "" {
		t.Errorf("a confirm card is expected: %s", f.api.lastReply(t))
	}
	f.assertNothingWritten(t, 1, 0)
	if got := f.mealTotal(); got != 150 {
		t.Errorf("meal total = %v, want unchanged 150", got)
	}
	if len(f.qa.asked) != 0 {
		t.Errorf("the QA model must not be asked when the planner produced a card: %q", f.qa.asked)
	}
}

// TestAffirmativeWithoutOfferFallsThrough [F34-07] 最後一輪不是提議時，閒聊的「好」照舊落到問答.
func TestAffirmativeWithoutOfferFallsThrough(t *testing.T) {
	f := newHonestyFixture(t)
	f.lastAnswer(honestyChat)
	f.sayAs(t, "U1", "好")
	if got := f.planner.calls.Load(); got != 0 {
		t.Errorf("planner calls = %d, want 0", got)
	}
	if len(f.qa.asked) != 1 {
		t.Errorf("the message must fall to QA, asked=%d", len(f.qa.asked))
	}
	// 沒有任何 history 時也一樣.
	g := newHonestyFixture(t)
	g.sayAs(t, "U1", "要")
	if got := g.planner.calls.Load(); got != 0 || len(g.qa.asked) != 1 {
		t.Errorf("without history: planner calls=%d asked=%d", got, len(g.qa.asked))
	}
}

// TestAffirmativeAfterOfferStillNeedsConfirm [F34-07] 回「要」只出卡，客人不按確認，紀錄就不會變.
func TestAffirmativeAfterOfferStillNeedsConfirm(t *testing.T) {
	f := newHonestyFixture(t)
	f.lastAnswer(honestyOffer)
	f.plan.answers = []string{planSetItem210}
	f.sayAs(t, "U1", "要")
	f.sayAs(t, "U1", "晚餐吃什麼比較好") // 不按確認、聊別的
	f.assertNothingWritten(t, 1, 0)
	if got := f.mealTotal(); got != 150 {
		t.Errorf("meal total = %v, want 150 until the customer presses confirm", got)
	}
	if f.store.Peek("U1") == nil {
		t.Error("the proposal must still be waiting for confirmation")
	}
}

// TestAffirmativeOfferConsumedOnce [F34-07] 同一個提議只換一次規劃器：出卡後再說「好」不叫規劃器、不蓋掉第一張卡.
func TestAffirmativeOfferConsumedOnce(t *testing.T) {
	f := newHonestyFixture(t)
	f.lastAnswer(honestyOffer)
	f.plan.answers = []string{planSetItem210, planSetItem210}
	f.sayAs(t, "U1", "要")
	first := f.store.Peek("U1")
	if first == nil || f.planner.calls.Load() != 1 {
		t.Fatalf("setup: first card missing (calls=%d)", f.planner.calls.Load())
	}
	nonce := first.Nonce
	f.sayAs(t, "U1", "好")
	if got := f.planner.calls.Load(); got != 1 {
		t.Errorf("planner calls = %d after the second affirmative, want still 1", got)
	}
	if p := f.store.Peek("U1"); p == nil || p.Nonce != nonce {
		t.Errorf("the first card must stay pending: %+v", p)
	}
	if len(f.qa.asked) != 1 {
		t.Errorf("the second affirmative must fall to QA, asked=%d", len(f.qa.asked))
	}
}

// TestEditGateRouteConsumesOffer [F34-07] 客人用「改」（走 EditGate）進來也消耗提議；額度已滿時不消耗也不叫規劃器.
func TestEditGateRouteConsumesOffer(t *testing.T) {
	t.Run("改 出卡後再說好不叫規劃器", func(t *testing.T) {
		f := newHonestyFixture(t)
		f.lastAnswer(honestyOffer)
		f.plan.answers = []string{planSetItem210, planSetItem210}
		f.sayAs(t, "U1", "改")
		if f.planner.calls.Load() != 1 || f.buttonData(t, "ok") == "" {
			t.Fatalf("setup: 改 must produce one planner call and a card (calls=%d)", f.planner.calls.Load())
		}
		nonce := f.store.Peek("U1").Nonce
		f.sayAs(t, "U1", "好")
		if got := f.planner.calls.Load(); got != 1 {
			t.Errorf("planner calls = %d, want still 1", got)
		}
		if p := f.store.Peek("U1"); p == nil || p.Nonce != nonce {
			t.Errorf("pending card must be unchanged: %+v", p)
		}
		if len(f.qa.asked) != 1 {
			t.Errorf("好 must fall to QA, asked=%d", len(f.qa.asked))
		}
	})
	t.Run("額度已滿不消耗也不叫規劃器", func(t *testing.T) {
		f := newHonestyFixture(t)
		f.lastAnswer(honestyOffer)
		key := "edit:count:U1:" + time.Now().In(nutrition.Taipei).Format("20060102")
		for range editDailyLimit {
			f.h.daily.Incr(key, time.Hour)
		}
		mustContain(t, f.sayAs(t, "U1", "要"), "上限")
		if !f.h.assistant.LastTurnOfferedEdit(context.Background(), "U1") {
			t.Error("a quota stop happens before PlanEdit, so the offer must not be consumed")
		}
		f.sayAs(t, "U1", "好")
		if got := f.planner.calls.Load(); got != 0 {
			t.Errorf("planner calls = %d, want 0 when the daily quota is full", got)
		}
		if f.store.Peek("U1") != nil {
			t.Error("no card may be created when the quota is full")
		}
	})
}
