package webhook

import "testing"

// [F37-01] 運動教學句只在文字含運動名時回；沒有運動名的閒聊（單字提示字、時間/距離字）改走問答.

// 含運動名、規則解析不出數量（或根本不認得）→ 整句教學句；AI 0 次、沒有紀錄、沒有狀態.
func TestWorkoutGuideRepliesWhenSportNamePresent(t *testing.T) {
	for _, text := range []string{
		"今天運動了一小時", // 運動
		"今天訓練了一小時", // 訓練
		"今天去健身",    // 健身
		"騎車去上班",    // 騎車（沒數量）
		"跑了一下",
		"打了一場球",
		"下班去划了二十分鐘船",
	} {
		t.Run(text, func(t *testing.T) {
			p := newBudgetProvider()
			p.qa = []string{budgetQAAnswer}
			f := newBudgetFixture(t, p)

			f.sayAs(t, "U1", text)

			if texts := lastReplyTexts(t, f); len(texts) != 1 || texts[0] != workoutGuideSentence {
				t.Errorf("reply texts = %q, want exactly the fixed guide", texts)
			}
			if p.totalCalls() != 0 {
				t.Errorf("AI calls = %d, want 0", p.totalCalls())
			}
			assertNoWorkoutSideEffects(t, f)
		})
	}
}

// 有運動名又有數量的句子照舊由關鍵字步驟直接記成運動紀錄（不回教學句、不進 AI）.
func TestSportNameWithAmountStillRecorded(t *testing.T) {
	for _, text := range []string{"騎腳踏車半小時", "今天重訓一小時"} {
		t.Run(text, func(t *testing.T) {
			p := newBudgetProvider()
			p.qa = []string{budgetQAAnswer}
			f := newBudgetFixture(t, p)

			f.sayAs(t, "U1", text)

			for _, s := range lastReplyTexts(t, f) {
				if s == workoutGuideSentence {
					t.Errorf("must not reply the guide: %q", s)
				}
			}
			if len(f.workouts.Items) != 1 {
				t.Errorf("workouts = %+v, want exactly one recorded", f.workouts.Items)
			}
			if p.totalCalls() != 0 {
				t.Errorf("AI calls = %d, want 0", p.totalCalls())
			}
		})
	}
}

// 沒有運動名的閒聊不回教學句，落到問答（回問答答案）；沒有運動紀錄、沒有狀態.
func TestNoSportNameFallsThroughToQA(t *testing.T) {
	for _, text := range []string{
		"路好難走",
		"我等了兩個小時",
		"今天練琴一小時", // 單字「練」不算運動名
		"騎樓好熱",    // 單字「騎」不算運動名
		"地球好大",
		"今天練了一下",
	} {
		t.Run(text, func(t *testing.T) {
			p := newBudgetProvider()
			p.qa = []string{budgetQAAnswer}
			f := newBudgetFixture(t, p)

			f.sayAs(t, "U1", text)

			texts := lastReplyTexts(t, f)
			if len(texts) == 0 || texts[0] == workoutGuideSentence {
				t.Fatalf("reply texts = %q, must not be the workout guide", texts)
			}
			if texts[0] != budgetQAAnswer {
				t.Errorf("reply texts = %q, want the QA answer %q", texts, budgetQAAnswer)
			}
			assertNoWorkoutSideEffects(t, f)
		})
	}
}

// 「記錄 」前綴的分支：沒有運動名的文字（路好難走）與沒有任何提示字的對照句（路好難）走同一條路，回覆與 AI 次數一致.
func TestExplicitLogWithoutSportNameMatchesControl(t *testing.T) {
	run := func(text string) ([]string, int) {
		p := newBudgetProvider()
		p.qa = []string{budgetQAAnswer}
		f := newBudgetFixture(t, p)
		f.sayAs(t, "U1", text)
		return lastReplyTexts(t, f), p.totalCalls()
	}
	gotTexts, gotCalls := run("記錄 路好難走")
	wantTexts, wantCalls := run("記錄 路好難")
	if len(gotTexts) != len(wantTexts) || (len(gotTexts) > 0 && gotTexts[0] != wantTexts[0]) {
		t.Errorf("replies differ: %q vs control %q", gotTexts, wantTexts)
	}
	if gotCalls != wantCalls {
		t.Errorf("AI calls %d vs control %d", gotCalls, wantCalls)
	}
}

// 「走路去吃牛肉麵」是記餐句：含運動別名「走路」但不是運動紀錄、不回教學句.
func TestWalkToEatSentenceIsNotWorkoutGuide(t *testing.T) {
	p := newBudgetProvider()
	p.qa = []string{budgetQAAnswer}
	f := newBudgetFixture(t, p)
	f.sayAs(t, "U1", "走路去吃牛肉麵")
	for _, s := range lastReplyTexts(t, f) {
		if s == workoutGuideSentence {
			t.Errorf("meal sentence got the workout guide: %q", s)
		}
	}
	if len(f.workouts.Items) != 0 {
		t.Errorf("workouts = %+v, want none", f.workouts.Items)
	}
}
