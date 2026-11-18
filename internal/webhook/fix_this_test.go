package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/intent"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// [F34-62] 批次回覆（一則訊息記多餐）加「修正這筆」，不含份量鈕；停用時沒有.
func TestBatchReplyOffersFixButton(t *testing.T) {
	f := newEditFixture(t, shotJSON)
	f.mealFixture.setNow(t, "2026-03-04 20:00")

	reply := f.say(t, screenshotText)

	if len(f.meals.Items) != 3 {
		t.Fatalf("meals = %d, want 3", len(f.meals.Items))
	}
	mustContain(t, reply, `"text":"修正這筆"`, `"type":"message"`, `"text":"取消這筆"`, `"text":"今日飲食"`, `"label":"拍下一餐"`)
	for _, unwanted := range []string{`"text":"只吃一半"`, `"text":"大份"`} {
		if strings.Contains(reply, unwanted) {
			t.Errorf("batch reply must not contain %s: %s", unwanted, reply)
		}
	}

	g := newMealFixture(t, shotJSON)
	g.setNow(t, "2026-03-04 20:00")
	if disabled := g.say(t, screenshotText); strings.Contains(disabled, `"text":"修正這筆"`) {
		t.Errorf("record edit disabled must not offer 修正這筆: %s", disabled)
	}
}

// [F34-62] 運動回覆：刪除這筆之外多一顆「修正這筆」；停用時沒有.
func TestWorkoutReplyOffersFixButton(t *testing.T) {
	f := newEditFixture(t)

	reply := f.say(t, "跑步 30分鐘")

	if len(f.workouts.Items) != 1 {
		t.Fatalf("workouts = %d, want 1", len(f.workouts.Items))
	}
	mustContain(t, reply, `"label":"刪除這筆"`, `"text":"修正這筆"`, `"type":"message"`)

	g := newFitnessFixture(t, "")
	if disabled := g.say(t, "跑步 30分鐘"); !strings.Contains(disabled, `"label":"刪除這筆"`) || strings.Contains(disabled, `"text":"修正這筆"`) {
		t.Errorf("record edit disabled: want 刪除這筆 only, got %s", disabled)
	}
}

// armedText 武裝後客人隨口說的、沒有修改動詞的一句.
const armedText = "麵包 210 大卡"

// newArmedFixture 聊天改紀錄啟用、近 7 天有一筆麵包 150 大卡.
func newArmedFixture(t *testing.T) (*editFixture, *meal.Meal) {
	t.Helper()
	f := newEditFixture(t)
	m := f.seedMeal(t, "U1", "麵包", 150, time.Now().Add(-time.Minute))
	if m.ID == "" {
		t.Fatal("seeded meal must have an ID")
	}
	return f, m
}

// stateOf U1 目前的對話狀態（沒有時 nil）.
func (f *editFixture) stateOf(t *testing.T) *conversation.State {
	t.Helper()
	st, err := f.h.convManager.Get(context.Background(), "U1")
	if err != nil {
		t.Fatalf("Get state: %v", err)
	}
	return st
}

func (f *editFixture) wantNoState(t *testing.T, when string) {
	t.Helper()
	if st := f.stateOf(t); st != nil {
		t.Errorf("%s: conversation state = %+v, want nil", when, st)
	}
}

// arm 按「修正這筆」並確認已武裝.
func (f *editFixture) arm(t *testing.T) string {
	t.Helper()
	reply := f.say(t, keywordFixThis)
	if st := f.stateOf(t); st == nil || st.Type != conversation.TypeRecordFix {
		t.Fatalf("after %s: state = %+v, want TypeRecordFix (reply %s)", keywordFixThis, st, reply)
	}
	return reply
}

func (f *editFixture) fixAnswers(answers ...string) {
	f.plan.mu.Lock()
	f.plan.answers = append(f.plan.answers, answers...)
	f.plan.mu.Unlock()
}

func (f *editFixture) itemKcal() float64 { return f.meals.Items[0].Items[0].Nutrients.Calories }

// [F34-63] 按「修正這筆」：回固定提示、武裝（只存型別，不含紀錄 ID 與文字）、不叫任何 AI、不扣額度、不寫暫存、不改紀錄.
func TestFixThisArmsStateAndShowsPromptWithoutAI(t *testing.T) {
	f, m := newArmedFixture(t)
	ic := &intentCompleter{answer: answerJSON(intent.None, "", "high"), failNow: true, t: t}
	f.h.WithIntent(intent.New(ic))

	reply := f.say(t, keywordFixThis)

	mustContain(t, reply, "要怎麼修正", "把午餐的麵包改成 210 大卡", "你按確認才會更新", `"label":"今日飲食"`)
	if strings.Contains(reply, "已改") {
		t.Errorf("the prompt must not claim success: %s", reply)
	}
	if n := f.planner.calls.Load(); n != 0 || len(f.qa.asked) != 0 || ic.count() != 0 || f.plan.Calls() != 0 {
		t.Errorf("planner=%d qa=%d intent=%d planAI=%d, want all 0", n, len(f.qa.asked), ic.count(), f.plan.Calls())
	}
	st := f.stateOf(t)
	if st == nil || st.Type != conversation.TypeRecordFix {
		t.Fatalf("state = %+v, want TypeRecordFix", st)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{m.ID, "麵包", keywordFixThis} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("state JSON %s must not contain %q", raw, leak)
		}
	}
	if f.store.Peek("U1") != nil {
		t.Error("no pending edit may be written")
	}
	if len(f.meals.Items) != 1 || f.itemKcal() != 150 {
		t.Errorf("meals changed: %+v", f.meals.Items)
	}
}

// [F34-64] 武裝後沒有修改動詞的一句直接進規劃器、出確認卡（不是新記一餐）；確認後才改；正對照：沒武裝同一句不進規劃器.
func TestFixThisThenVerblessSentenceGoesToPlanner(t *testing.T) {
	f, _ := newArmedFixture(t)
	f.fixAnswers(planSetItem210)
	f.arm(t)

	card := f.say(t, armedText)

	mustContain(t, card, "ce1:ok:")
	if n := f.planner.calls.Load(); n != 1 {
		t.Errorf("planner calls = %d, want 1", n)
	}
	if len(f.meals.Items) != 1 || len(f.ai.reqs) != 0 || f.itemKcal() != 150 {
		t.Errorf("no new meal, no estimate, item unchanged until confirm: meals=%d estimates=%d kcal=%v", len(f.meals.Items), len(f.ai.reqs), f.itemKcal())
	}
	f.wantNoState(t, "after the planned sentence")

	mustContain(t, f.say(t, "確認"), "已改")
	if f.itemKcal() != 210 {
		t.Errorf("after confirm kcal = %v, want 210", f.itemKcal())
	}
	f.wantNoState(t, "after confirm")

	// 正對照：沒按「修正這筆」直接送同一句，不進規劃器、沒有確認卡.
	g, _ := newArmedFixture(t)
	g.fixAnswers(planSetItem210)
	reply := g.say(t, armedText)
	if n := g.planner.calls.Load(); n != 0 || strings.Contains(reply, "ce1:ok:") {
		t.Errorf("control (not armed): planner=%d reply=%s", n, reply)
	}
}

// [F34-64] 武裝狀態用一次就清：出卡後同一句再送不再進規劃器；規劃器回 None 時狀態也清、訊息落回後面步驟.
func TestFixThisStateIsOneShot(t *testing.T) {
	t.Run("card then same sentence again", func(t *testing.T) {
		f, _ := newArmedFixture(t)
		f.fixAnswers(planSetItem210, planSetItem210)
		f.arm(t)
		mustContain(t, f.say(t, armedText), "ce1:ok:")
		f.wantNoState(t, "after first sentence")
		f.say(t, armedText)
		if n := f.planner.calls.Load(); n != 1 {
			t.Errorf("planner calls = %d, want still 1 (second sentence is not armed)", n)
		}
		f.wantNoState(t, "after second sentence")
	})
	t.Run("planner says none", func(t *testing.T) {
		f, _ := newArmedFixture(t)
		f.fixAnswers(planNone)
		f.arm(t)
		reply := f.say(t, armedText) // 必須有回覆（say 沒回覆會 fatal）
		if n := f.planner.calls.Load(); n != 1 {
			t.Errorf("planner calls = %d, want 1", n)
		}
		f.wantNoState(t, "after planner none")
		// 落回後面的步驟：問答、估算記餐至少有一個被走到.
		if len(f.qa.asked) == 0 && len(f.ai.reqs) == 0 && len(f.meals.Items) == 1 {
			t.Errorf("the sentence must fall through to meal/QA handling, reply: %s", reply)
		}
	})
}

// [F34-63] 武裝 5 分鐘過期（注入時鐘，不真的等）.
func TestFixThisStateExpiresAfterFiveMinutes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		advance  time.Duration
		wantCard bool
	}{
		{"4 minutes later is still armed", 4 * time.Minute, true},
		{"6 minutes later is expired", 6 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newArmedFixture(t)
			now := time.Now()
			f.h.convManager = conversation.NewMemoryManagerWithClock(func() time.Time { return now })
			f.fixAnswers(planSetItem210)
			f.arm(t)
			now = now.Add(tc.advance)

			reply := f.say(t, armedText)

			wantCalls := int64(0)
			if tc.wantCard {
				wantCalls = 1
			}
			if n := f.planner.calls.Load(); n != wantCalls || strings.Contains(reply, "ce1:ok:") != tc.wantCard {
				t.Errorf("planner=%d card=%v, want planner=%d card=%v: %s", n, strings.Contains(reply, "ce1:ok:"), wantCalls, tc.wantCard, reply)
			}
		})
	}
}

// fixPassThroughRows [F34-64] 直通清單的逐項預期（手列：10 個選單指令、說明別名，各自的既有回覆）.
var fixPassThroughRows = []struct{ text, want string }{
	{keywordTodayMeals, "牛肉麵"},
	{keywordCancelMeal, "已取消這筆紀錄"},
	{keywordMealPlace, "在哪"},
	{keywordHelp, "我可以幫你"},
	{"使用說明", "我可以幫你"},
	{"功能", "我可以幫你"},
	{"選單", "我可以幫你"},
	{"help", "我可以幫你"},
	{"?", "我可以幫你"},
	{"？", "我可以幫你"},
	{keywordWeather, "輸入地名"},
	{keywordSnapFood, "拍一張餐點照片"},
	{keywordLogWorkout, "做了什麼運動"},
	{keywordToday, "今日"},
	{keywordWeek, "本週"},
	{keywordAsk, "想問什麼都可以"},
	{keywordDeleteMeal, "已刪除"},
}

// [F34-64] 武裝後選單指令照常處理並清掉武裝：不進規劃器、不扣額度；表外的「麵包 210 大卡」進規劃器.
func TestFixThisPassThroughMenuCommands(t *testing.T) {
	// 表要涵蓋 isMenuCommand 的 10 個指令與全部 helpAliases（手列，不靠反射）.
	covered := map[string]bool{}
	for _, r := range fixPassThroughRows {
		covered[r.text] = true
	}
	for _, c := range []string{keywordWeather, keywordSnapFood, keywordLogWorkout, keywordToday, keywordWeek, keywordAsk,
		keywordTodayMeals, keywordDeleteMeal, keywordCancelMeal, keywordMealPlace} {
		if !covered[c] {
			t.Errorf("menu command %q missing from the table", c)
		}
	}
	for alias := range helpAliases {
		if !covered[alias] {
			t.Errorf("help alias %q missing from the table", alias)
		}
	}

	for _, r := range fixPassThroughRows {
		t.Run(r.text, func(t *testing.T) {
			f := newEditFixture(t, noodleText)
			f.say(t, "午餐吃了牛肉麵")
			f.arm(t)

			reply := f.say(t, r.text)

			mustContain(t, reply, r.want)
			if n := f.planner.calls.Load(); n != 0 {
				t.Errorf("%q must not reach the planner, calls = %d", r.text, n)
			}
			if r.text == keywordMealPlace {
				// 「在哪吃的」本身會開店家問答的對話狀態；重點是武裝已被它取代.
				if st := f.stateOf(t); st == nil || st.Type != conversation.TypeMealPlace {
					t.Errorf("state = %+v, want TypeMealPlace (armed state replaced)", st)
				}
				return
			}
			f.wantNoState(t, "after "+r.text)
		})
	}

	// 對照：表外的句子進規劃器.
	f := newEditFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")
	f.fixAnswers(planNone)
	f.arm(t)
	f.say(t, armedText)
	if n := f.planner.calls.Load(); n != 1 {
		t.Errorf("control sentence planner calls = %d, want 1", n)
	}
}

// [F34-64] 武裝後其餘一律進規劃器（規劃器回 None 後各自原本的處理仍發生）；「確認刪除」是對照列（先被過期提示接走）.
func TestFixThisNonMenuInputsGoToPlannerThenFallThrough(t *testing.T) {
	rows := []struct {
		text  string
		check func(t *testing.T, f *editFixture, reply string, mealsBefore int)
	}{
		{"只吃一半", func(t *testing.T, f *editFixture, reply string, _ int) {
			mustContain(t, reply, "已改成一半的份量")
			if strings.Contains(reply, "ce1:ok:") {
				t.Errorf("must not be a confirm card: %s", reply)
			}
		}},
		{"記錄：牛肉麵", func(t *testing.T, f *editFixture, reply string, before int) {
			mustContain(t, reply, "已記錄")
			if len(f.meals.Items) != before+1 {
				t.Errorf("meals = %d, want %d", len(f.meals.Items), before+1)
			}
		}},
		{"這個熱量準嗎？", func(t *testing.T, f *editFixture, _ string, _ int) {
			if len(f.qa.asked) != 1 {
				t.Errorf("qa asked = %d, want 1", len(f.qa.asked))
			}
		}},
		{"記錄", func(t *testing.T, _ *editFixture, reply string, _ int) {
			mustContain(t, reply, "直接傳吃了什麼")
		}},
	}
	for _, r := range rows {
		t.Run(r.text, func(t *testing.T) {
			f := newEditFixture(t, noodleText, noodleText)
			f.say(t, "午餐吃了水餃")
			before := len(f.meals.Items)
			f.fixAnswers(planNone)
			f.arm(t)
			calls := f.planner.calls.Load()

			reply := f.say(t, r.text)

			if n := f.planner.calls.Load(); n != calls+1 {
				t.Errorf("planner calls = %d, want %d", n, calls+1)
			}
			f.wantNoState(t, "after "+r.text)
			r.check(t, f, reply, before)
		})
	}

	// 對照：同一句不武裝時不叫規劃器.
	g := newEditFixture(t, noodleText)
	g.say(t, "午餐吃了水餃")
	g.say(t, "只吃一半")
	if n := g.planner.calls.Load(); n != 0 {
		t.Errorf("control (not armed) 只吃一半 planner calls = %d, want 0", n)
	}

	// 「確認刪除」：stepStaleDeleteConfirm 排在 conversation 之前，只回過期提示、不進規劃器、武裝留到 TTL.
	t.Run("確認刪除", func(t *testing.T) {
		f, _ := newArmedFixture(t)
		f.arm(t)
		reply := f.say(t, keywordConfirmDelete)
		mustContain(t, reply, staleDeleteConfirmMessage)
		if n := f.planner.calls.Load(); n != 0 {
			t.Errorf("planner calls = %d, want 0", n)
		}
		if st := f.stateOf(t); st == nil || st.Type != conversation.TypeRecordFix {
			t.Errorf("state = %+v, want TypeRecordFix kept until TTL", st)
		}
	})
}

// [F34-64] 已有確認卡時再按「修正這筆」，下一句打「確認」直通、由打字確認套用舊卡，不進規劃器；「取消」同理.
func TestFixThisArmedConfirmAppliesPendingCardWithoutPlanner(t *testing.T) {
	f, _ := newArmedFixture(t)
	f.proposeAndGetOK(t, "U1", "把麵包改成 210 大卡", planSetItem210)
	card := f.store.Peek("U1")
	if card == nil || f.planner.calls.Load() != 1 {
		t.Fatalf("setup: pending=%v planner=%d", card, f.planner.calls.Load())
	}
	f.arm(t)
	if now := f.store.Peek("U1"); now == nil || now.Nonce != card.Nonce {
		t.Fatalf("arming must not touch the pending card: %+v", now)
	}

	reply := f.say(t, "確認")

	mustContain(t, reply, "已改")
	if f.itemKcal() != 210 || f.planner.calls.Load() != 1 {
		t.Errorf("kcal=%v planner=%d, want 210 and still 1", f.itemKcal(), f.planner.calls.Load())
	}
	f.wantNoState(t, "after typed confirm")
	if f.store.Peek("U1") != nil {
		t.Error("the card must be consumed")
	}

	// 打字「取消」：卡被取消、品項不變.
	g, _ := newArmedFixture(t)
	g.proposeAndGetOK(t, "U1", "把麵包改成 210 大卡", planSetItem210)
	g.arm(t)
	mustContain(t, g.say(t, "取消"), editCancelledMessage)
	if g.itemKcal() != 150 || g.planner.calls.Load() != 1 || g.store.Peek("U1") != nil {
		t.Errorf("cancel: kcal=%v planner=%d pending=%v", g.itemKcal(), g.planner.calls.Load(), g.store.Peek("U1"))
	}
	g.wantNoState(t, "after typed cancel")
}

// [F34-64] 武裝後沒有待確認卡就打「確認」「取消」：固定句、不進規劃器、不問答.
func TestFixThisArmedConfirmWithoutCardRepliesFixedLine(t *testing.T) {
	for _, text := range []string{"確認", "取消"} {
		t.Run(text, func(t *testing.T) {
			f, _ := newArmedFixture(t)
			f.arm(t)

			reply := f.say(t, text)

			mustContain(t, reply, editNoPendingMessage)
			if n := f.planner.calls.Load(); n != 0 || len(f.qa.asked) != 0 {
				t.Errorf("planner=%d qa=%d, want 0", n, len(f.qa.asked))
			}
			f.wantNoState(t, "after "+text)
			if len(f.meals.Items) != 1 || f.itemKcal() != 150 {
				t.Errorf("meals changed: %+v", f.meals.Items)
			}
		})
	}
	// 對照：沒按「修正這筆」直接送「確認」，回覆相同（既有行為）.
	g, _ := newArmedFixture(t)
	mustContain(t, g.say(t, "確認"), editNoPendingMessage)
}

// [F34-64] 武裝期間再按一次「修正這筆」：重新武裝並再回提示，不進規劃器.
func TestFixThisPressAgainRearms(t *testing.T) {
	f, _ := newArmedFixture(t)
	f.arm(t)

	reply := f.arm(t)

	mustContain(t, reply, "要怎麼修正")
	if n := f.planner.calls.Load(); n != 0 || len(f.qa.asked) != 0 {
		t.Errorf("planner=%d qa=%d, want 0", n, len(f.qa.asked))
	}
}

// [F34-64] 非文字訊息（照片）清掉武裝；純空白訊息不動狀態（留到 TTL）.
func TestFixThisClearedByNonTextAndKeptByBlank(t *testing.T) {
	t.Run("photo clears", func(t *testing.T) {
		f, _ := newArmedFixture(t)
		f.arm(t)
		f.photo(t, bentoPhoto)
		f.wantNoState(t, "after a photo")
		f.say(t, armedText)
		if n := f.planner.calls.Load(); n != 0 {
			t.Errorf("planner calls = %d, want 0 (the armed state was cleared by the photo)", n)
		}
	})
	t.Run("blank keeps", func(t *testing.T) {
		f, _ := newArmedFixture(t)
		f.arm(t)
		mustContain(t, f.say(t, "   "), "輸入「說明」")
		if st := f.stateOf(t); st == nil || st.Type != conversation.TypeRecordFix {
			t.Errorf("state = %+v, want TypeRecordFix kept", st)
		}
	})
}

// [F34-63] 對話管理不可用（Redis 之外也沒有記憶體）：只回提示、不假裝武裝、不 panic；之後的句子照舊.
func TestFixThisDegradesWithoutConversationManager(t *testing.T) {
	f, _ := newArmedFixture(t)
	f.h.convManager = &conversation.Manager{}
	if f.h.convManager.Available() {
		t.Fatal("positive control: the zero-value manager must report unavailable")
	}

	mustContain(t, f.say(t, keywordFixThis), "要怎麼修正")
	f.say(t, armedText)
	if n := f.planner.calls.Load(); n != 0 {
		t.Errorf("planner calls = %d, want 0 (prompt-only degrade, not armed)", n)
	}
	f.fixAnswers(planSetItem210)
	mustContain(t, f.say(t, "把麵包改成 210 大卡"), "ce1:ok:")
}

// [F34-63] 聊天改紀錄停用：不回提示、不武裝、照舊落到問答.
func TestFixThisDisabledFallsThrough(t *testing.T) {
	f := newMealFixture(t)

	reply := f.say(t, keywordFixThis)

	if strings.Contains(reply, "要怎麼修正") {
		t.Errorf("disabled feature must not promise editing: %s", reply)
	}
	if st, _ := f.h.convManager.Get(context.Background(), "U1"); st != nil {
		t.Errorf("state = %+v, want nil", st)
	}
	if len(f.qa.asked) != 1 {
		t.Errorf("qa asked = %d, want 1 (same as before this change)", len(f.qa.asked))
	}
}

// [F34-63] 沒有可修正的紀錄：回固定句、不叫規劃器、不武裝；seed 一筆後才武裝（見 TestFixThisArmsStateAndShowsPromptWithoutAI）.
func TestFixThisWithoutRecords(t *testing.T) {
	f := newEditFixture(t)

	reply := f.say(t, keywordFixThis)

	mustContain(t, reply, "目前沒有可以修正的紀錄")
	if n := f.planner.calls.Load(); n != 0 {
		t.Errorf("planner calls = %d, want 0", n)
	}
	f.wantNoState(t, "without records")
}

// [F34-63] 只有全等才武裝：含其他字的句子走 EditGate／規劃器.
func TestFixThisExactMatchOnly(t *testing.T) {
	f, _ := newArmedFixture(t)
	f.fixAnswers(planNone, planNone)
	for _, text := range []string{"修正這筆熱量改成 500 大卡", "修正這筆。"} {
		reply := f.say(t, text)
		if strings.Contains(reply, "要怎麼修正") {
			t.Errorf("%q must not arm: %s", text, reply)
		}
		f.wantNoState(t, text)
	}
	if n := f.planner.calls.Load(); n != 2 {
		t.Errorf("planner calls = %d, want 2 (one per sentence)", n)
	}
}

// [F34-64] 追問中（meal_clarify）點「修正這筆」＝換話題：追問狀態被清、改成武裝，沒有新餐被記；再按會重新武裝.
func TestFixThisFromMealClarifyRearmsAfterTopicChange(t *testing.T) {
	f := newEditFixture(t, bentoClarify)
	f.seedMeal(t, "U1", "麵包", 150, time.Now().Add(-time.Minute))

	f.say(t, "晚餐吃便當")
	if st := f.stateOf(t); st == nil || st.Type != conversation.TypeMealClarify {
		t.Fatalf("setup: state = %+v, want TypeMealClarify", st)
	}
	if len(f.meals.Items) != 1 {
		t.Fatalf("setup: meals = %d, want only the seeded one", len(f.meals.Items))
	}

	reply := f.say(t, keywordFixThis)

	mustContain(t, reply, "要怎麼修正")
	if st := f.stateOf(t); st == nil || st.Type != conversation.TypeRecordFix {
		t.Errorf("state = %+v, want TypeRecordFix", st)
	}
	if len(f.meals.Items) != 1 {
		t.Errorf("no new meal may be recorded, meals = %d", len(f.meals.Items))
	}
}
