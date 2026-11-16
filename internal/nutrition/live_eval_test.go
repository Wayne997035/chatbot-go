//go:build liveeval

package nutrition

// 真模型評測：同一道菜換做法、份量、加料，熱量要跟著變；不能是固定值.
// 平常的 go test 與 CI 不會編進來；手動執行（金鑰放在自己的憑證目錄，用 *_API_KEY_FILE 指過去）：
//   ANTHROPIC_API_KEY_FILE=<金鑰檔> go test -tags liveeval ./internal/nutrition/ -run TestLive -v -count=1
// 金鑰只在程式內使用，不印出；一把都沒有直接失敗.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chatbot-go/internal/ai"
	"chatbot-go/internal/platform/config"
)

func liveAnalyzer(t *testing.T) *Analyzer {
	t.Helper()
	c := liveAIConfig(t)
	client := &http.Client{Timeout: 60 * time.Second}
	r := ai.NewRouter(
		&ai.Settings{
			TextOrder:   config.SplitList(c.ProviderOrder),
			VisionOrder: config.SplitList(c.VisionProviderOrder),
		},
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
	var items []string
	for _, f := range est.Foods {
		items = append(items, f.Name+"／"+f.Portion+"／"+strconvF(f.Calories))
	}
	t.Logf("%s → 合計 %s 大卡（%s）", text, strconvF(est.Total.Calories), strings.Join(items, "；"))
	l.cache[text] = est.Total.Calories
	return est.Total.Calories
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
	}
	for _, p := range pairs {
		a, b := l.kcal(t, p[0]), l.kcal(t, p[1])
		if a >= b {
			t.Errorf("「%s」%.0f 應少於「%s」%.0f", p[0], a, p[1], b)
		}
	}
	// 合理範圍（台灣常見份量的寬鬆區間）：離譜的估算直接失敗.
	bounds := []struct {
		text     string
		min, max float64
	}{
		{"點心吃了蘋果 一顆", 50, 160},
		{"晚餐吃了白飯 一碗", 200, 380},
		{"午餐吃了水餃 10 顆", 350, 800},
		{"早餐吃了茶葉蛋 一顆", 55, 110},
		{"下午喝了珍珠奶茶 大杯 全糖", 400, 950},
		{"晚餐吃了雞排 一份", 400, 950},
		{"早餐喝了美式咖啡 中杯", 0, 40},
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
		t.Logf("只寫牛肉麵 第 %d 次：%.0f 大卡", i+1, est.Total.Calories)
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
	}
	if c.GeminiAPIKey == "" && c.GroqAPIKey == "" && c.NVIDIAAPIKey == "" && c.AnthropicAPIKey == "" {
		t.Fatal("沒有任何 AI 金鑰：設定 ANTHROPIC_API_KEY_FILE / GEMINI_API_KEY_FILE / GROQ_API_KEY_FILE / NVIDIA_API_KEY_FILE 指向金鑰檔")
	}
	return c
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
