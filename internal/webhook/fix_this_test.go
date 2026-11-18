package webhook

import (
	"strings"
	"testing"
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
