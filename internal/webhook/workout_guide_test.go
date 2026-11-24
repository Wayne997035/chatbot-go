package webhook

import (
	"context"
	"testing"
)

// [F36-05] 運動拔 AI 之後 stepWorkout 的教學句行為.
// 用「真」ai.Router + 假 Provider（newBudgetFixture）：任何 AI 呼叫（規劃器 / 記餐 / 意圖 / 問答）都會被 budgetProvider 計次.

const (
	// workoutGuideSentence 規格 F1/F7 的固定句，整句全等（決策 12c6e8e6）；刻意不引用 workoutGuideMessage，避免常數被改時測試跟著變.
	workoutGuideSentence = "可以這樣記：跑步 30 分鐘、游泳 1 公里"
	// quickReplyQuestion assistant_handler.go 的快速回覆原文「今天適合跑步嗎？」.
	quickReplyQuestion = "今天適合跑步嗎？"
)

// assertNoWorkoutSideEffects 沒有運動紀錄、沒有對話狀態.
func assertNoWorkoutSideEffects(t *testing.T, f *editFixture) {
	t.Helper()
	if len(f.workouts.Items) != 0 {
		t.Errorf("workouts = %+v, want none inserted", f.workouts.Items)
	}
	st, err := f.h.convManager.Get(context.Background(), "U1")
	if err != nil || st != nil {
		t.Errorf("conversation state = %+v err=%v, want none", st, err)
	}
}

// F1：規則解析不出、含運動提示字的句子回固定教學句；AI 0 次、沒有 Insert、沒有狀態、沒落問答.
func TestUnparsedWorkoutRepliesFixedGuide(t *testing.T) {
	for _, text := range []string{"打了一場球", "下班去划了二十分鐘船"} {
		t.Run(text, func(t *testing.T) {
			p := newBudgetProvider()
			p.qa = []string{budgetQAAnswer} // 若誤落問答，回覆會變成這句、計數會 ≥1
			f := newBudgetFixture(t, p)

			f.sayAs(t, "U1", text)

			if texts := lastReplyTexts(t, f); len(texts) != 1 || texts[0] != workoutGuideSentence {
				t.Errorf("reply texts = %q, want exactly the fixed guide", texts)
			}
			if p.totalCalls() != 0 || p.count("qa") != 0 {
				t.Errorf("AI calls total=%d qa=%d, want 0/0", p.totalCalls(), p.count("qa"))
			}
			assertNoWorkoutSideEffects(t, f)
		})
	}
}

// F2：快速回覆原文「今天適合跑步嗎？」是問句 → 不回教學句，照舊走問答，最後回問答答案.
func TestWorkoutGuideSkipsQuestions(t *testing.T) {
	p := newBudgetProvider()
	p.qa = []string{budgetQAAnswer}
	f := newBudgetFixture(t, p)

	f.sayAs(t, "U1", quickReplyQuestion)

	texts := lastReplyTexts(t, f)
	if len(texts) == 0 || texts[0] == workoutGuideSentence || texts[0] != budgetQAAnswer {
		t.Errorf("reply texts = %q, want the QA answer %q and no guide", texts, budgetQAAnswer)
	}
	if p.count("qa") != 1 {
		t.Errorf("qa calls = %d, want 1", p.count("qa"))
	}
}

// F6：「每小時提醒我」含運動提示字「小時」+ 意圖提示字「提醒」、不是問句 → 不回教學句，進意圖（計 1）.
func TestWorkoutGuideSkipsIntentHintMessages(t *testing.T) {
	p := newBudgetProvider()
	p.qa = []string{budgetQAAnswer}
	f := newBudgetFixture(t, p)

	f.sayAs(t, "U1", "每小時提醒我")

	if p.count("intent") != 1 {
		t.Errorf("intent calls = %d, want 1 (the message must reach the intent step)", p.count("intent"))
	}
	for _, s := range lastReplyTexts(t, f) {
		if s == workoutGuideSentence {
			t.Errorf("reply must not be the workout guide: %q", s)
		}
	}
}

// F7（A3）：有運動名但沒數量（打了一場球、跑了一下）同回教學句整句全等；無 Insert、無狀態、AI 0 次、問答 0 次.
func TestNoAmountWorkoutRepliesFixedGuide(t *testing.T) {
	for _, text := range []string{"打了一場球", "跑了一下"} {
		t.Run(text, func(t *testing.T) {
			p := newBudgetProvider()
			p.qa = []string{budgetQAAnswer}
			f := newBudgetFixture(t, p)

			f.sayAs(t, "U1", text)

			if texts := lastReplyTexts(t, f); len(texts) != 1 || texts[0] != workoutGuideSentence {
				t.Errorf("reply texts = %q, want exactly the fixed guide", texts)
			}
			if p.totalCalls() != 0 || p.count("qa") != 0 {
				t.Errorf("AI calls total=%d qa=%d, want 0/0", p.totalCalls(), p.count("qa"))
			}
			assertNoWorkoutSideEffects(t, f)
		})
	}
}

// F7：只打運動名（IsActivityOnly）仍追問「跑步多久呢？」，不回教學句（既有行為不變）.
func TestActivityOnlyStillAsksDuration(t *testing.T) {
	p := newBudgetProvider()
	f := newBudgetFixture(t, p)

	f.sayAs(t, "U1", "跑步")

	texts := lastReplyTexts(t, f)
	if len(texts) != 1 || texts[0] != "跑步多久呢？" {
		t.Errorf("reply texts = %q, want the duration question", texts)
	}
	if p.totalCalls() != 0 {
		t.Errorf("AI calls = %d, want 0", p.totalCalls())
	}
}
