//go:build liveeval

package intent

// [F33-18] 分類器真模型表（L10）：照正式環境的供應商設定呼叫真的 AI，檢查「改紀錄」句進得了分類器且被判 edit_record，
// 一般問答與新記錄句不會被誤判成改紀錄. 平常的 go test 與 CI 不會編進來；手動執行（金鑰放在自己的憑證目錄，用 *_API_KEY_FILE 指過去）：
//   ANTHROPIC_API_KEY_FILE=<金鑰檔> go test -tags liveeval ./internal/intent/ -run TestLive -v -count=1
// 金鑰只在程式內使用，不印出；一把都沒有直接失敗. 判紅讀 PassesGate 與 Result.Intent/Confidence，不對文字做關鍵字計數.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chatbot-go/internal/ai"
	"chatbot-go/internal/platform/config"
)

// 正例必須進得了閘門且被判 edit_record+high（通過率 ≥ 90%）；反例任何一句被判 edit_record+high 都算紅.
var (
	editPositives = []string{
		"你要直接更新正確啦",
		"這個麵包其實夾了起司和火腿",
		"午餐那筆熱量記太少了",
		"把剛剛的便當刪掉",
		"跑步時間記錄錯了應該是 30 分鐘",
		"早餐那個蛋餅少算了一顆蛋",
		"晚餐的熱量算錯了，幫我改成 600 大卡",
		"剛剛那筆運動的距離不對，應該是 5 公里",
		"把今天的早餐刪除",
		"這個便當我只吃了一半，幫我改成一半的熱量",
		"麵包裡面有加起司，熱量應該比 100 高",
		"請更新成你剛剛估的數字",
	}
	editNegatives = []string{
		"牛肉麵多少大卡？",
		"今天吃了多少熱量",
		"晚餐吃什麼比較好",
		"我午餐吃了牛肉麵",
		"跑步 5公里 30分鐘",
		"大安區天氣",
		"幫我開啟天氣特報",
		"這個熱量高嗎",
		"珍奶半糖大概幾大卡",
		"我今天早餐吃了蛋餅和豆漿",
		"幫我記錄一下晚餐吃了雞腿便當",
		"明天會下雨嗎",
	}
)

// editClassified 一句話經過閘門與分類器的結果.
type editClassified struct {
	text string
	gate bool
	res  Result
	err  error
}

func (c *editClassified) isEdit() bool {
	return c.gate && c.err == nil && c.res.Intent == EditRecord && c.res.Confidence == ConfidenceHigh
}

// judgeEditClassify 判紅函式（真模型表與正對照共用）：回傳每一條紅的原因，空 = 全綠.
func judgeEditClassify(positives, negatives []editClassified) []string {
	var reds []string
	pass := 0
	for i := range positives {
		if !positives[i].gate {
			reds = append(reds, "正例進不了分類器（PassesGate=false）："+positives[i].text)
		}
		if positives[i].isEdit() {
			pass++
		}
	}
	if len(positives) > 0 && pass*10 < len(positives)*9 {
		reds = append(reds, "正例通過率低於 90%")
	}
	for i := range negatives {
		if negatives[i].isEdit() {
			reds = append(reds, "反例被判成 edit_record+high："+negatives[i].text)
		}
	}
	return reds
}

func classifyAll(t *testing.T, c *Classifier, texts []string) []editClassified {
	t.Helper()
	out := make([]editClassified, 0, len(texts))
	for _, text := range texts {
		item := editClassified{text: text, gate: PassesGate(text)}
		if item.gate {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			item.res, item.err = c.Classify(ctx, text)
			cancel()
		}
		t.Logf("%q gate=%v intent=%s confidence=%s err=%v", text, item.gate, item.res.Intent, item.res.Confidence, item.err)
		out = append(out, item)
	}
	return out
}

// TestLiveEditClassify L10：分類器真模型表.
func TestLiveEditClassify(t *testing.T) {
	cfg := liveClassifierConfig(t)
	client := &http.Client{Timeout: 60 * time.Second}
	r := ai.NewRouter(
		&ai.Settings{TextOrder: config.SplitList(cfg.ProviderOrder), VisionOrder: config.SplitList(cfg.VisionProviderOrder)},
		ai.NewGemini(cfg.GeminiAPIKey, client),
		ai.NewGroq(cfg.GroqAPIKey, client),
		ai.NewNVIDIA(cfg.NVIDIAAPIKey, client),
		ai.NewClaude(cfg.AnthropicAPIKey, cfg.ClaudeModel),
	)
	if len(r.Configured()) == 0 {
		t.Fatal("金鑰都無效，沒有可用的 AI 供應商")
	}
	c := New(r)
	for _, red := range judgeEditClassify(classifyAll(t, c, editPositives), classifyAll(t, c, editNegatives)) {
		t.Errorf("紅：%s", red)
	}
}

// TestLiveEditClassifyHoldCheck 證明判紅函式會叫（不呼叫模型、不需要金鑰）：反例被判 edit_record 要叫、正例通過率 80% 要叫、
// 正例進不了閘門要叫；全綠與剛好 90% 不得誤叫.
func TestLiveEditClassifyHoldCheck(t *testing.T) {
	edit := Result{Intent: EditRecord, Confidence: ConfidenceHigh}
	other := Result{Intent: None, Confidence: ConfidenceLow}
	build := func(n, passing int, gate bool) []editClassified {
		out := make([]editClassified, n)
		for i := range out {
			out[i] = editClassified{text: "句子", gate: gate, res: other}
			if i < passing {
				out[i].res = edit
			}
		}
		return out
	}
	negOK := build(10, 0, true)
	if reds := judgeEditClassify(build(10, 10, true), negOK); len(reds) != 0 {
		t.Errorf("全綠不得叫：%v", reds)
	}
	if reds := judgeEditClassify(build(10, 9, true), negOK); len(reds) != 0 {
		t.Errorf("正例通過率剛好 90%% 不得叫：%v", reds)
	}
	if reds := judgeEditClassify(build(10, 8, true), negOK); len(reds) == 0 {
		t.Error("正例通過率 80% 必須叫")
	}
	if reds := judgeEditClassify(build(10, 10, false), negOK); len(reds) == 0 {
		t.Error("正例進不了閘門必須叫")
	}
	neg := build(10, 0, true)
	neg[3].res = edit
	if reds := judgeEditClassify(build(10, 10, true), neg); len(reds) != 1 || !strings.Contains(reds[0], "反例") {
		t.Errorf("反例被判 edit_record+high 必須叫：%v", reds)
	}
	neg[3].res = Result{Intent: EditRecord, Confidence: ConfidenceLow}
	if reds := judgeEditClassify(build(10, 10, true), neg); len(reds) != 0 {
		t.Errorf("反例被判 edit_record 但信心 low 不算紅（只有 high 才會叫規劃器）：%v", reds)
	}
	failed := build(10, 10, true)
	failed[0].err = ErrUnavailable
	if reds := judgeEditClassify(failed, negOK); len(reds) != 0 {
		t.Errorf("1/10 分類失敗仍在 90%% 門檻內，不得叫：%v", reds)
	}
	if len(editPositives) < 10 || len(editNegatives) < 10 {
		t.Errorf("每組至少 10 句：正例 %d、反例 %d", len(editPositives), len(editNegatives))
	}
	for _, p := range editPositives {
		if !PassesGate(p) {
			t.Errorf("正例進不了閘門（真模型表會紅）：%q", p)
		}
	}
}

// liveClassifierConfig 金鑰一律從 <NAME>_FILE 指到的檔案讀，程式只在記憶體裡用、不印出；刻意跑真模型卻一把金鑰都沒有時直接失敗.
func liveClassifierConfig(t *testing.T) *config.AIConfig {
	t.Helper()
	c := &config.AIConfig{
		ProviderOrder:       os.Getenv("AI_PROVIDER_ORDER"),
		VisionProviderOrder: os.Getenv("AI_VISION_PROVIDER_ORDER"),
		ClaudeModel:         os.Getenv("CLAUDE_MODEL"),
		AnthropicAPIKey:     liveKey(t, "ANTHROPIC_API_KEY_FILE"),
		GeminiAPIKey:        liveKey(t, "GEMINI_API_KEY_FILE"),
		GroqAPIKey:          liveKey(t, "GROQ_API_KEY_FILE"),
		NVIDIAAPIKey:        liveKey(t, "NVIDIA_API_KEY_FILE"),
	}
	if c.GeminiAPIKey == "" && c.GroqAPIKey == "" && c.NVIDIAAPIKey == "" && c.AnthropicAPIKey == "" {
		t.Fatal("沒有任何 AI 金鑰：設定 ANTHROPIC_API_KEY_FILE / GEMINI_API_KEY_FILE / GROQ_API_KEY_FILE / NVIDIA_API_KEY_FILE 指向金鑰檔")
	}
	return c
}

func liveKey(t *testing.T, env string) string {
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
