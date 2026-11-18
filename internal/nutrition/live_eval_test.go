//go:build liveeval

package nutrition

// 真模型評測：同一道菜換做法、份量、加料，熱量要跟著變；不能是固定值.
// 平常的 go test 與 CI 不會編進來；手動執行（金鑰放在自己的憑證目錄，用 *_API_KEY_FILE 指過去）：
//   ANTHROPIC_API_KEY_FILE=<金鑰檔> go test -tags liveeval ./internal/nutrition/ -run TestLive -v -count=1
// 金鑰只在程式內使用，不印出；一把都沒有直接失敗.

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/platform/config"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func liveAnalyzer(t *testing.T) *Analyzer {
	t.Helper()
	c := liveAIConfig(t)
	client := &http.Client{Timeout: 60 * time.Second}
	settings := liveSettings(c)
	r := ai.NewRouter(
		&settings,
		ai.NewGemini(c.GeminiAPIKey, client),
		ai.NewGroq(c.GroqAPIKey, client),
		ai.NewNVIDIA(c.NVIDIAAPIKey, client),
		ai.NewClaude(c.AnthropicAPIKey, c.ClaudeModel),
	)
	if len(r.Configured()) == 0 {
		t.Fatal("金鑰都無效，沒有可用的 AI 供應商")
	}
	t.Logf("可用供應商: %s；文字順序: %s", strings.Join(r.Configured(), ","), c.ProviderOrder)
	return NewAnalyzer(r)
}

// liveEstimate 每段描述只問一次（同一段描述重複出現時沿用第一次的結果）.
type liveEstimate struct {
	an    *Analyzer
	cache map[string]float64
}

func (l *liveEstimate) kcal(t *testing.T, text string) float64 {
	t.Helper()
	if v, ok := l.cache[text]; ok {
		return v
	}
	est, err := l.an.EstimateText(context.Background(), text, false)
	if err != nil {
		t.Fatalf("%q: %v", text, err)
	}
	// [F34-28] TextEstimate 改多餐：價差評測的句子都是單一餐，這裡把所有餐的食物與熱量加總.
	var items []string
	var total float64
	for i := range est.Meals {
		for _, f := range est.Meals[i].Foods {
			items = append(items, f.Name+"／"+f.Portion+"／"+strconvF(f.Calories))
		}
		total += est.Meals[i].Total.Calories
	}
	t.Logf("%s → 合計 %s 大卡（%s）", text, strconvF(total), strings.Join(items, "；"))
	l.cache[text] = total
	return total
}

// TestLiveVariantsChangeCalories 做法、份量、杯型、甜度、加料、配菜都要反映在熱量上；不能每次都給同一個固定值.
func TestLiveVariantsChangeCalories(t *testing.T) {
	l := &liveEstimate{an: liveAnalyzer(t), cache: map[string]float64{}}
	// 每組：前者應明顯少於後者.
	pairs := [][2]string{
		{"午餐吃了紅燒牛肉麵 小碗", "午餐吃了紅燒牛肉麵 大碗"},
		{"午餐吃了紅燒牛肉麵 大碗", "午餐吃了紅燒牛肉麵 大碗 加麵"},
		{"午餐吃了紅燒牛肉麵 大碗", "午餐吃了紅燒牛肉麵 大碗 還吃了一盤滷豆干海帶小菜"},
		{"午餐吃了清燉牛肉麵 大碗", "午餐吃了紅燒牛肉麵 大碗"},
		{"晚餐吃了白飯 半碗", "晚餐吃了白飯 一碗"},
		{"晚餐吃了白飯 一碗", "晚餐吃了白飯 兩碗"},
		{"午餐吃了水餃 5 顆", "午餐吃了水餃 10 顆"},
		{"午餐吃了水餃 10 顆", "午餐吃了水餃 15 顆"},
		{"午餐吃了滷肉飯 小碗", "午餐吃了滷肉飯 大碗"},
		{"晚餐吃了雞排 半份", "晚餐吃了雞排 一份"},
		{"點心吃了蘋果 一顆", "點心吃了蘋果 兩顆"},
		{"早餐吃了黑麥酸種麵包 一片約 50 克", "早餐吃了黑麥酸種麵包 一片約 100 克"},
		{"下午喝了珍珠奶茶 大杯 無糖", "下午喝了珍珠奶茶 大杯 全糖"},
		{"下午喝了珍珠奶茶 中杯 半糖", "下午喝了珍珠奶茶 大杯 半糖"},
		{"下午喝了奶茶 大杯 半糖", "下午喝了珍珠奶茶 大杯 半糖"},
		{"早餐喝了美式咖啡 中杯", "早餐喝了拿鐵 中杯"},
		{"早餐喝了拿鐵 中杯", "早餐喝了拿鐵 大杯"},
		{"早餐吃了原味蛋餅", "早餐吃了起司蛋餅"},
		{"午餐吃了生菜沙拉 不加醬", "午餐吃了生菜沙拉 加凱撒醬"},
		// 提示詞參考值沒涵蓋的品項：確認是真的分析，不是只對參考值有反應.
		{"午餐吃了鍋貼 5 顆", "午餐吃了鍋貼 10 顆"},
		{"晚餐吃了蛋炒飯 半盤", "晚餐吃了蛋炒飯 一盤"},
		{"宵夜吃了鹹酥雞 小份", "宵夜吃了鹹酥雞 大份"},
		{"午餐吃了雞腿便當", "午餐吃了雞腿便當 加一顆滷蛋"},
		{"早餐喝了豆漿 中杯 無糖", "早餐喝了豆漿 中杯 全糖"},
	}
	for _, p := range pairs {
		a, b := l.kcal(t, p[0]), l.kcal(t, p[1])
		if a >= b {
			t.Errorf("「%s」%.0f 應少於「%s」%.0f", p[0], a, p[1], b)
		}
	}
	// 合理範圍（台灣常見份量的寬鬆區間）：離譜的估算直接失敗.
	// 合理範圍（出處：雞排約 630、珍奶 700ml 無糖 440／全糖 650 —— 營養師高敏敏、台大醫院營養室；
	// 豬肉水餃一顆 50～64；紅燒牛肉麵一碗約 870～963 —— 營養師高敏敏、李婉萍）；其餘為寬鬆常識區間.
	bounds := []struct {
		text     string
		min, max float64
	}{
		{"點心吃了蘋果 一顆", 50, 160},
		{"午餐吃了水餃 10 顆", 450, 700},
		{"早餐吃了茶葉蛋 一顆", 55, 110},
		{"下午喝了珍珠奶茶 大杯 全糖", 550, 750},
		{"下午喝了珍珠奶茶 大杯 無糖", 350, 520},
		{"晚餐吃了雞排 一份", 500, 750},
		{"早餐喝了美式咖啡 中杯", 0, 40},
		{"午餐吃了紅燒牛肉麵", 700, 1050},
	}
	for _, b := range bounds {
		if v := l.kcal(t, b.text); v < b.min || v > b.max {
			t.Errorf("「%s」%.0f 不在合理範圍 %.0f-%.0f", b.text, v, b.min, b.max)
		}
	}
	// 只寫「牛肉麵」連問三次：記錄估法是否穩定（只記錄，不判定）.
	for i := 0; i < 3; i++ {
		est, err := l.an.EstimateText(context.Background(), "午餐吃了牛肉麵", false)
		if err != nil {
			t.Fatal(err)
		}
		var total float64
		for k := range est.Meals {
			total += est.Meals[k].Total.Calories
		}
		t.Logf("只寫牛肉麵 第 %d 次：%.0f 大卡", i+1, total)
	}
}

func strconvF(v float64) string { return fmt.Sprintf("%.0f", v) }

// liveAIConfig 金鑰一律從 <NAME>_FILE 指到的檔案讀（集中放在使用者自己的憑證目錄），程式只在記憶體裡用、不印出；
// 供應商順序與模型照 Render 同名的環境變數，沒設就用程式預設值（和正式環境沒設時一樣）.
// 刻意跑真模型卻一把金鑰都沒有時直接失敗，不准 SKIP 成假綠燈.
func liveAIConfig(t *testing.T) *config.AIConfig {
	t.Helper()
	c := &config.AIConfig{
		ProviderOrder:       os.Getenv("AI_PROVIDER_ORDER"),
		VisionProviderOrder: os.Getenv("AI_VISION_PROVIDER_ORDER"),
		QAProviderOrder:     os.Getenv("AI_QA_PROVIDER_ORDER"),
		ClaudeModel:         os.Getenv("CLAUDE_MODEL"),
		AnthropicAPIKey:     keyFromFile(t, "ANTHROPIC_API_KEY_FILE"),
		GeminiAPIKey:        keyFromFile(t, "GEMINI_API_KEY_FILE"),
		GroqAPIKey:          keyFromFile(t, "GROQ_API_KEY_FILE"),
		NVIDIAAPIKey:        keyFromFile(t, "NVIDIA_API_KEY_FILE"),
		// 與 configs/production.yaml 同名的環境變數.
		NVIDIAModels:            os.Getenv("NVIDIA_MODELS"),
		GroqModels:              os.Getenv("GROQ_MODELS"),
		GeminiModels:            os.Getenv("GEMINI_MODELS"),
		AttemptTimeoutSeconds:   os.Getenv("AI_ATTEMPT_TIMEOUT_SECONDS"),
		ProviderBudgetSeconds:   os.Getenv("AI_PROVIDER_BUDGET_SECONDS"),
		ProviderCooldownSeconds: os.Getenv("AI_PROVIDER_COOLDOWN_SECONDS"),
		RateLimitWaitSeconds:    os.Getenv("AI_RATE_LIMIT_WAIT_SECONDS"),
	}
	if c.GeminiAPIKey == "" && c.GroqAPIKey == "" && c.NVIDIAAPIKey == "" && c.AnthropicAPIKey == "" {
		t.Fatal("沒有任何 AI 金鑰：設定 ANTHROPIC_API_KEY_FILE / GEMINI_API_KEY_FILE / GROQ_API_KEY_FILE / NVIDIA_API_KEY_FILE 指向金鑰檔")
	}
	return c
}

// liveSettings 照正式 app.aiSettings 的對應組路由設定（順序、模型清單、四個秒數），
// 重播測的才是正式會用的模型與時限；0 值交給 ai 套件補預設，與正式相同.
func liveSettings(c *config.AIConfig) ai.Settings {
	s := c.Settings()
	models := map[string][]string{}
	for key, list := range map[string][]string{
		ai.ProviderNVIDIA: s.Models["nvidia"],
		ai.ProviderGroq:   s.Models["groq"],
		ai.ProviderGemini: s.Models["gemini"],
	} {
		if len(list) > 0 {
			models[key] = list
		}
	}
	return ai.Settings{
		TextOrder:        config.SplitList(c.ProviderOrder),
		VisionOrder:      config.SplitList(c.VisionProviderOrder),
		Models:           models,
		AttemptTimeout:   s.AttemptTimeout,
		ProviderBudget:   s.ProviderBudget,
		ProviderCooldown: s.ProviderCooldown,
		RateLimitWait:    s.RateLimitWait,
	}
}

// keyFromFile 讀環境變數指到的金鑰檔；沒設回空字串，設了卻讀不到直接失敗.
func keyFromFile(t *testing.T, env string) string {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("%s 指到的檔案讀不到（不印內容）: %v", env, err)
	}
	return strings.TrimSpace(string(b))
}

// liveMealCase [F34-28] 一則多餐描述的真模型期望；只判欄位（餐數、餐別、DayOffset、IsFood、Clarify），不對文字做關鍵字計數.
type liveMealCase struct {
	text         string
	allowClarify bool
	ok           func(e *TextEstimate) bool
}

func liveTypes(e *TextEstimate) []MealType {
	out := make([]MealType, len(e.Meals))
	for i := range e.Meals {
		out[i] = e.Meals[i].MealType
	}
	return out
}

func liveTypesAre(e *TextEstimate, want ...MealType) bool {
	got := liveTypes(e)
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func liveDaysAre(e *TextEstimate, want ...int) bool {
	if len(e.Meals) != len(want) {
		return false
	}
	for i := range want {
		if e.Meals[i].DayOffset != want[i] {
			return false
		}
	}
	return true
}

// TestLiveMultiMeal [F34-28] 一則訊息多餐：餐數與餐別、day_offset 套到每一餐. 每句跑 3 次，整體通過率 ≥ 90%.
func TestLiveMultiMeal(t *testing.T) {
	an := liveAnalyzer(t)
	cases := []liveMealCase{
		{"早餐吃 牛肉火腿起司三明治\n\n午餐 黑麥酸種麵包兩個手掌大小\n\n晚餐 福隆雞腿便當 加上哈根達斯草莓口味4匙 加上一杯牛奶", false, func(e *TextEstimate) bool {
			return e.IsFood && liveTypesAre(e, Breakfast, Lunch, Dinner) && liveDaysAre(e, 0, 0, 0) && len(e.Meals[2].Foods) >= 3
		}},
		{"早上吃了蛋餅，中午吃了便當，下午吃了蛋糕，晚上吃了火鍋", false, func(e *TextEstimate) bool {
			return e.IsFood && liveTypesAre(e, Breakfast, Lunch, Snack, Dinner)
		}},
		{"昨天早餐吃了蛋餅，晚餐吃了拉麵", false, func(e *TextEstimate) bool { return e.IsFood && len(e.Meals) == 2 && liveDaysAre(e, -1, -1) }},
		{"昨天中午吃了便當，今天晚上吃了火鍋", false, func(e *TextEstimate) bool { return e.IsFood && len(e.Meals) == 2 && liveDaysAre(e, -1, 0) }},
		{"晚餐吃了拉麵，宵夜吃了鹹酥雞", false, func(e *TextEstimate) bool { return e.IsFood && liveTypesAre(e, Dinner, LateNight) }},
		{"午餐吃了牛肉麵", false, func(e *TextEstimate) bool { return e.IsFood && liveTypesAre(e, Lunch) }},
		{"晚餐吃了便當加一杯牛奶", false, func(e *TextEstimate) bool { return e.IsFood && len(e.Meals) == 1 }},
		{"早餐吃便當，午餐吃了牛肉麵", true, func(e *TextEstimate) bool { return e.IsFood && (e.Clarify != nil || len(e.Meals) == 2) }},
		{"吃了閉門羹", false, func(e *TextEstimate) bool { return !e.IsFood }},
	}
	passed, total := 0, 0
	for _, c := range cases {
		for run := 1; run <= 3; run++ {
			total++
			est, err := an.EstimateText(context.Background(), c.text, c.allowClarify)
			if err != nil {
				t.Errorf("紅：%q 第 %d 次：%v", c.text, run, err)
				continue
			}
			ok := c.ok(est)
			if ok {
				passed++
			}
			t.Logf("%q 第 %d 次 ok=%v 餐別=%v dayOffsets=%d clarify=%v", c.text, run, ok, liveTypes(est), len(est.Meals), est.Clarify != nil)
		}
	}
	if passed*10 < total*9 {
		t.Errorf("紅：通過率 %d/%d 低於 90%%", passed, total)
	}
}

// TestLiveSnackRule [F34-80] 沒說餐別且整筆只有零食飲料 → 點心：判定欄位取 ResolveMeals 的餐別序列、Inferred 與 DayOffset.
// 每句跑 3 次，整體通過率 ≥ 90%；不達不合入. 只在 user 本機跑真模型，CI 不編進來.
func TestLiveSnackRule(t *testing.T) {
	an := liveAnalyzer(t)
	at := func(h, min int) time.Time { return time.Date(2026, 3, 4, h, min, 0, 0, Taipei) }
	cases := []struct {
		text string
		now  time.Time
		ok   func(e *TextEstimate, got []MealEstimate) bool
	}{
		{"喝了一杯珍珠奶茶", at(10, 30), func(_ *TextEstimate, got []MealEstimate) bool { return liveResolved(got, Snack, true) }},
		{"喝了一杯珍珠奶茶", at(20, 0), func(_ *TextEstimate, got []MealEstimate) bool { return liveResolved(got, Snack, true) }},
		{"吃了兩顆水煎包", at(8, 0), func(_ *TextEstimate, got []MealEstimate) bool { return liveResolved(got, Snack, true) }},
		{"吃了一包洋芋片和幾片餅乾", at(12, 0), func(_ *TextEstimate, got []MealEstimate) bool { return liveResolved(got, Snack, true) }},
		{"吃了一包餅乾", at(23, 30), func(_ *TextEstimate, got []MealEstimate) bool { return liveResolved(got, Snack, true) }},
		{"早餐喝咖啡", at(20, 0), func(_ *TextEstimate, got []MealEstimate) bool { return liveResolved(got, Breakfast, false) }},
		{"晚餐喝了珍奶", at(10, 0), func(_ *TextEstimate, got []MealEstimate) bool { return liveResolved(got, Dinner, false) }},
		{"點心吃了雞排", at(20, 0), func(_ *TextEstimate, got []MealEstimate) bool { return liveResolved(got, Snack, false) }},
		{"吃了三明治和拿鐵", at(8, 0), func(_ *TextEstimate, got []MealEstimate) bool {
			return len(got) >= 1 && got[0].MealType == Breakfast
		}},
		{"吃了雞排配珍奶", at(20, 0), func(_ *TextEstimate, got []MealEstimate) bool { return len(got) == 1 && got[0].MealType == Dinner }},
		{"喝了一杯珍奶，吃了一碗牛肉麵", at(20, 0), func(_ *TextEstimate, got []MealEstimate) bool {
			return len(got) == 1 && got[0].MealType == Dinner
		}},
		{"午餐吃了便當配珍奶", at(20, 0), func(_ *TextEstimate, got []MealEstimate) bool { return len(got) == 1 && got[0].MealType == Lunch }},
		{"昨天喝了珍珠奶茶", at(20, 0), func(_ *TextEstimate, got []MealEstimate) bool {
			return liveResolved(got, Snack, true) && got[0].DayOffset == -1
		}},
		{"吃了閉門羹", at(12, 0), func(e *TextEstimate, _ []MealEstimate) bool { return !e.IsFood }},
	}
	passed, total := 0, 0
	for _, c := range cases {
		for run := 1; run <= 3; run++ {
			total++
			est, err := an.EstimateText(context.Background(), c.text, false)
			if err != nil {
				t.Errorf("紅：%q 第 %d 次：%v", c.text, run, err)
				continue
			}
			got := ResolveMeals(c.now, est.Meals)
			ok := c.ok(est, got)
			if ok {
				passed++
			}
			t.Logf("%q（現在 %s）第 %d 次 ok=%v isFood=%v 餐別=%v", c.text, c.now.Format("15:04"), run, ok, est.IsFood, liveTypes(&TextEstimate{Meals: got}))
		}
	}
	if passed*10 < total*9 {
		t.Errorf("紅：通過率 %d/%d 低於 90%%", passed, total)
	}
}

// liveResolved ResolveMeals 的結果恰一餐，餐別與 Inferred 都符合.
func liveResolved(got []MealEstimate, want MealType, inferred bool) bool {
	return len(got) == 1 && got[0].MealType == want && got[0].Inferred == inferred
}
