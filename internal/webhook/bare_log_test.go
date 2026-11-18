package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/intent"
	"chatbot-go/internal/weather"
	"context"
	"strings"
	"testing"
)

// bareLogTwelve [F34-60] 規格的 12 句封閉集合（手列，預期值不取自實作）.
var bareLogTwelve = []string{
	"記錄", "登錄", "記錄一下", "登錄一下", "幫我記錄", "幫我登錄", "幫我記錄一下", "幫我登錄一下",
	"記錄進去", "登錄進去", "幫我記錄進去", "幫我登錄進去",
}

// assertBareLogGuide 回覆是指引（含拍照鈕），且沒有叫任何 AI、沒有寫入.
func assertBareLogGuide(t *testing.T, f *mealFixture, ic *intentCompleter, input, reply string) {
	t.Helper()
	mustContain(t, reply, "直接傳吃了什麼", `"label":"拍下一餐"`)
	if ic.count() != 0 || len(f.qa.asked) != 0 || len(f.ai.reqs) != 0 || len(f.meals.Items) != 0 {
		t.Errorf("%q: intent=%d qa=%d estimates=%d meals=%d, want all 0", input, ic.count(), len(f.qa.asked), len(f.ai.reqs), len(f.meals.Items))
	}
}

// [F34-60] 單打「記錄」12 句（含句尾標點）回固定指引：意圖 AI、問答、估算都不被叫，不寫入.
func TestBareLogCommandShowsGuide(t *testing.T) {
	if len(bareLogCommands) != 12 {
		t.Errorf("len(bareLogCommands) = %d, want exactly 12", len(bareLogCommands))
	}
	f, ic := newMealIntentFixture(t, answerJSON(intent.LogMeal, "", "high"), noodleText, noodleText)
	ic.failNow = true
	for _, text := range append(append([]string{}, bareLogTwelve...), "記錄。", "登錄！", "  幫我登錄進去~ ") {
		t.Run(text, func(t *testing.T) {
			reply := f.say(t, text)
			assertBareLogGuide(t, f, ic, text, reply)
		})
	}
	// 對照：不在集合的「記錄牛肉麵」不回指引（走原路徑；這裡要允許意圖 AI 被叫，所以另開設備）.
	g, _ := newMealIntentFixture(t, answerJSON(intent.None, "", "high"), noodleText)
	if reply := g.say(t, "記錄牛肉麵"); strings.Contains(reply, "直接傳吃了什麼") {
		t.Errorf("記錄牛肉麵 must not show the guide: %s", reply)
	}
}

// [F34-60] 沒有 WithNutrition／WithIntent／WithAssistant 的 handler 也要回指引.
func TestBareLogGuideWithAllAIOff(t *testing.T) {
	api := newLineAPI(t, jpegHeader)
	h := NewWebhookHandler(api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil, conversation.NewMemoryManager())

	h.handleTextMessage(context.Background(), "U1", "記錄", "tok")
	if len(api.replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(api.replies))
	}
	mustContain(t, api.lastReply(t), "直接傳吃了什麼", `"label":"拍下一餐"`)

	// 對照：同 handler 送「查天氣」不回指引.
	h.handleTextMessage(context.Background(), "U1", keywordWeather, "tok")
	if got := api.lastReply(t); strings.Contains(got, "直接傳吃了什麼") {
		t.Errorf("查天氣 must not show the guide: %s", got)
	}
}

// [F34-60] 有內容的記錄句不被指引吞掉（行為與改動前相同）.
func TestBareLogDoesNotSwallowContent(t *testing.T) {
	f := newMealFixture(t, noodleText, noodleText, noodleText, noodleText)
	for i, text := range []string{"記錄：牛肉麵", "記錄 滷肉飯", "記錄一下我午餐吃了水餃", "幫我記錄今天午餐吃了雞腿便當"} {
		reply := f.say(t, text)
		mustContain(t, reply, "已記錄")
		if strings.Contains(reply, "直接傳吃了什麼") {
			t.Errorf("%q must log a meal, not show the guide: %s", text, reply)
		}
		if len(f.meals.Items) != i+1 {
			t.Errorf("after %q meals = %d, want %d", text, len(f.meals.Items), i+1)
		}
	}

	g := newFitnessFixture(t, "")
	reply := g.say(t, "記錄 跑步 5公里")
	if len(g.workouts.Items) != 1 || strings.Contains(reply, "直接傳吃了什麼") {
		t.Errorf("記錄 跑步 5公里: workouts=%d reply=%s", len(g.workouts.Items), reply)
	}
	reply = g.say(t, "記運動")
	mustContain(t, reply, "做了什麼運動")
	if len(g.workouts.Items) != 1 || strings.Contains(reply, "直接傳吃了什麼") {
		t.Errorf("記運動: workouts=%d reply=%s", len(g.workouts.Items), reply)
	}

	// 對照：「記錄牛肉麵」走原路徑（記餐），不是指引.
	h := newMealFixture(t, noodleText)
	if reply := h.say(t, "記錄牛肉麵"); strings.Contains(reply, "直接傳吃了什麼") {
		t.Errorf("記錄牛肉麵 must not show the guide: %s", reply)
	}
}

// [F34-60] 句尾語助詞與表情符號去除後仍是 12 句之一才回指引；句中有內容的不回.
func TestBareLogGuideIgnoresTrailingParticlesAndEmoji(t *testing.T) {
	f, ic := newMealIntentFixture(t, answerJSON(intent.LogMeal, "", "high"), noodleText, noodleText, noodleText, noodleText)
	ic.failNow = true
	hits := []string{
		"幫我記錄一下喔", "記錄😊", "記錄啦～", "記錄啦啦", "幫我登錄進去吧！", "登錄一下唷😊",
		"記錄 ❤️", "記錄👍🏽", "記錄😊😊", "幫我記錄哦。", "記錄：", "記錄啊…",
	}
	for _, text := range hits {
		t.Run(text, func(t *testing.T) {
			reply := f.say(t, text)
			assertBareLogGuide(t, f, ic, text, reply)
		})
	}

	// 對照：這些不是單打記錄，不得回指引（intent 可被叫，所以換不 failNow 的設備）.
	g, _ := newMealIntentFixture(t, answerJSON(intent.None, "", "high"), noodleText, noodleText, noodleText, noodleText)
	for _, text := range []string{"記錄牛肉麵喔", "記錄😊牛肉麵", "喔記錄", "記錄一下好了"} {
		if reply := g.say(t, text); strings.Contains(reply, "直接傳吃了什麼") {
			t.Errorf("%q must not show the guide: %s", text, reply)
		}
	}
}

// [F34-60] 句尾去除表：命中與否、去尾結果恰等於集合中的一句.
func TestBareLogSuffixTrimTable(t *testing.T) {
	cases := []struct {
		in  string
		hit bool
	}{
		{"記錄", true}, {"記錄😊", true}, {"記錄喔", true}, {"記錄喔喔", true}, {"記錄啦～", true}, {"記錄。", true},
		{"記錄 ", true}, {"記錄👨‍👩‍👧", true}, {"記錄吧啦", true},
		{"記錄😊牛肉麵", false}, {"記錄牛肉麵", false}, {"記錄一下我午餐吃了牛肉麵喔", false},
		{"喔", false}, {"😊", false}, {"記錄好", false},
		// 只去句尾：句首與句中的語助詞／表情不能被去掉（否則「喔記錄」「記錄😊一下」會變成命中）.
		{"喔記錄", false}, {"😊記錄", false}, {"記錄😊一下", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			trimmed := trimBareLogSuffix(tc.in)
			if got := bareLogCommands[trimmed]; got != tc.hit {
				t.Fatalf("hit(%q) = %v (trimmed %q), want %v", tc.in, got, trimmed, tc.hit)
			}
			if got := isBareLogCommand(tc.in); got != tc.hit {
				t.Fatalf("isBareLogCommand(%q) = %v, want %v", tc.in, got, tc.hit)
			}
			if tc.hit {
				found := false
				for _, c := range bareLogTwelve {
					found = found || c == trimmed
				}
				if !found {
					t.Errorf("trimmed %q is not one of the 12 sentences", trimmed)
				}
			}
		})
	}
}

// [F34-60] 句中有內容、只是尾巴帶語助詞／表情的句子照常記餐（以原文處理，不傳去尾後的字串）.
func TestBareLogParticleSentenceWithContentStillLogsMeal(t *testing.T) {
	f, ic := newMealIntentFixture(t, answerJSON(intent.None, "", "high"), noodleText, noodleText, noodleText)
	ic.failNow = true
	for i, text := range []string{"記錄一下我午餐吃了牛肉麵喔", "幫我記錄今天午餐吃了雞腿便當啦", "記錄：滷肉飯😊"} {
		reply := f.say(t, text)
		mustContain(t, reply, "已記錄")
		if strings.Contains(reply, "直接傳吃了什麼") {
			t.Errorf("%q must log a meal: %s", text, reply)
		}
		if len(f.meals.Items) != i+1 {
			t.Errorf("after %q meals = %d, want %d", text, len(f.meals.Items), i+1)
		}
	}
	if ic.count() != 0 {
		t.Errorf("intent AI called %d times, want 0", ic.count())
	}
	// 估算收到的是原文，不是去尾後的字串.
	if len(f.ai.reqs) == 0 {
		t.Fatal("no estimate request was made, the meals were not logged through the estimator")
	}
	if !strings.Contains(f.ai.reqs[0].Prompt, "喔") {
		t.Errorf("estimate must receive the original text, prompt: %s", f.ai.reqs[0].Prompt)
	}
	// 對照：「記錄喔」回指引、meals 不增加.
	before := len(f.meals.Items)
	mustContain(t, f.say(t, "記錄喔"), "直接傳吃了什麼")
	if len(f.meals.Items) != before {
		t.Errorf("meals grew on 記錄喔: %d -> %d", before, len(f.meals.Items))
	}
}
