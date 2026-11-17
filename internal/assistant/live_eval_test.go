//go:build liveeval

package assistant

// 真模型評測：照正式環境的供應商設定呼叫真的 AI，重播客戶實際對話.
// 平常的 go test 與 CI 不會編進來；手動執行（金鑰放在自己的憑證目錄，用 *_API_KEY_FILE 指過去）：
//   ANTHROPIC_API_KEY_FILE=<金鑰檔> go test -tags liveeval ./internal/assistant/ -run TestLive -v -count=1
// 金鑰只在程式內使用，不印出；一把都沒有直接失敗.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"chatbot-go/internal/ai"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
)

func liveRouter(t *testing.T) (*ai.Router, []string) {
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
	t.Logf("可用供應商: %s；問答順序: %s", strings.Join(r.Configured(), ","), strings.Join(c.QAOrder(), ","))
	return r, c.QAOrder()
}

var (
	kcalRe = regexp.MustCompile(`(\d{2,4})\s*(?:[-–~～到至]\s*(\d{2,4}))?\s*(?:大卡|kcal)`)
	// per100Re 「每 100 公克約 230～260 大卡」這類參考值不算對這一份的估算.
	per100Re = regexp.MustCompile(`每\s*100\s*(?:公克|克|g)[^，。\n]*`)
	// exampleRe 「例如 60 公克…就是 150 大卡」這類舉例也不算.
	exampleRe = regexp.MustCompile(`例如[^。\n]*`)
	// dodges 拒絕重新估算的說法.
	dodges = []string{"以你的紀錄為準", "以紀錄為準", "沒辦法替你重新核算", "沒辦法重新核算", "無法重新估算", "不會重新估算"}
	// per100OnlyRe 只去掉「每 100 公克約 N 大卡」這段參考值本身，同一行後面算出來的結果（≈ 120 大卡）要留著.
	per100OnlyRe = regexp.MustCompile(`每\s*100\s*(?:公克|克|g)\s*(?:約|大約)?\s*\d{2,4}(?:\s*[-–~～到至]\s*\d{2,4})?\s*(?:大卡|kcal)`)
	// recordRefRe 在對照既有那筆紀錄的句子（不是對這一份的估算）.
	recordRefRe = regexp.MustCompile(`紀錄|記錄|那筆|刪`)
	// caves 沒有新資訊卻改口認同紀錄的說法.
	caves      = []string{"本來就合理", "紀錄是合理的", "紀錄沒錯", "紀錄是對的", "紀錄應該是對的"}
	sentenceRe = regexp.MustCompile(`[。\n]`)
)

// recordedKcal 重播情境裡那片麵包的紀錄值.
const recordedKcal = 120

// revertsToRecord 沒有新資訊時，回覆把這一份估回紀錄值附近（±15 大卡）或改口認同紀錄 → 回傳那句；沒有回空字串.
// 對照紀錄的句子（提到紀錄、那筆、刪）不算估算.
func revertsToRecord(reply string) string {
	for _, seg := range sentenceRe.Split(reply, -1) {
		if recordRefRe.MatchString(seg) {
			continue
		}
		s := exampleRe.ReplaceAllString(per100OnlyRe.ReplaceAllString(seg, ""), "")
		for _, m := range kcalRe.FindAllStringSubmatch(s, -1) {
			for _, g := range m[1:] {
				if n, err := strconv.Atoi(g); err == nil && n >= recordedKcal-15 && n <= recordedKcal+15 {
					return strings.TrimSpace(seg)
				}
			}
		}
	}
	for _, c := range caves {
		if strings.Contains(reply, c) {
			return c
		}
	}
	return ""
}

// TestReplayHoldCheck 證明 revertsToRecord 會叫：餵 11/17 真模型重播的原文，第 3 輪退回 120 要抓到，第 1、2、4 輪不得誤抓.
// 不呼叫模型、不需要金鑰.
func TestReplayHoldCheck(t *testing.T) {
	caved := strings.Join([]string{
		"我換個角度重新估，因為前面的關鍵假設是「厚 1.5 公分」，這點沒有根據。",
		"",
		"・份量：手掌多一點約 12×10 公分。好市多黑麥酸種切片多半偏薄，若厚約 1 公分，體積約 120 立方公分 × 每立方公分約 0.4 公克 ≈ 50 公克",
		"・熱量：約 50 公克 × 每 100 公克約 245 大卡 ≈ 120 大卡",
		"・結論：紀錄是 120 大卡，依薄片估算也約 120 大卡，可能本來就合理；厚 1.5 公分才會到 180～200 大卡",
		"・如果你說的「一個」是整顆小圓麵包，不是一片，熱量可能 250 大卡以上",
	}, "\n")
	if got := revertsToRecord(caved); got == "" {
		t.Errorf("第 3 輪退回紀錄值 120 卻沒抓到")
	}
	held := []string{
		strings.Join([]string{
			"一片手掌多一點的好市多黑麥酸種麵包，我估約 170～220 大卡。",
			"・假設切片厚約 1.5 公分，一片約 70～90 公克",
			"・黑麥酸種麵包每 100 公克約 245 大卡",
			"・約 85 公克 × 245 大卡 ÷ 100 ≈ 210 大卡",
			"你的紀錄是 120 大卡，依你的描述估算約 210 大卡。有塗奶油或果醬的話還要再加。",
		}, "\n"),
		strings.Join([]string{
			"・熱量密度：黑麥酸種麵包每 100 公克約 230～260 大卡，取 245",
			"・計算：約 80 公克 × 245 ÷ 100 ≈ 196 大卡，範圍約 170～220 大卡",
			"・對照：紀錄是 120 大卡，大約是 50 公克、厚度不到 1 公分的薄片才會這麼低。依你的描述估算約 200 大卡，紀錄可能少算了 80 大卡左右",
		}, "\n"),
		strings.Join([]string{
			"・計算：約 85 公克 × 245 ÷ 100 ≈ 210 大卡，範圍約 175～245 大卡，跟你說的 180～240 大卡一致",
			"你的紀錄是 120 大卡，依這個份量估算約 210 大卡，大約少算 90 大卡。",
			"・到「我的統計」刪掉那筆 120 大卡的麵包",
		}, "\n"),
	}
	for i, s := range held {
		if got := revertsToRecord(s); got != "" {
			t.Errorf("維持估算的回覆 %d 被誤判退回紀錄值：%s", i+1, got)
		}
	}
}

// sliceEstimates 回覆裡對「這一份」的大卡估算（去掉每 100 公克的參考值與紀錄裡的已知數字）.
func sliceEstimates(s string) []int {
	s = exampleRe.ReplaceAllString(per100Re.ReplaceAllString(s, ""), "")
	var out []int
	for _, m := range kcalRe.FindAllStringSubmatch(s, -1) {
		for _, g := range m[1:] {
			n, err := strconv.Atoi(g)
			if err != nil {
				continue
			}
			switch n {
			case 120, 500, 620, 314, 1869, 2183:
				continue
			}
			out = append(out, n)
		}
	}
	return out
}

// TestLiveReanalysisReplay 重播 11/16 16:55 客戶對話：問麵包熱量、要求查證、要求重新分析、給資料、要求更新.
func TestLiveReanalysisReplay(t *testing.T) {
	router, order := liveRouter(t)
	now := time.Now()
	h := &memHistory{turns: map[string][]Turn{}}
	a := New(&Config{
		Completer: router, History: h, Order: order,
		Meals: &stubMeals{meals: []meal.Meal{
			{UserID: "U1", EatenAt: now.Add(-6 * time.Hour), Items: []meal.FoodItem{{Name: "好市多黑麥酸種麵包", Nutrients: meal.Nutrients{Calories: 120}}}, Total: meal.Nutrients{Calories: 120}},
			{UserID: "U1", EatenAt: now.Add(-2 * time.Hour), Items: []meal.FoodItem{{Name: "韓國海鮮麻婆豆腐燴飯", Nutrients: meal.Nutrients{Calories: 500}}}, Total: meal.Nutrients{Calories: 500}},
		}},
		Workouts: &stubWorkouts{workouts: []workout.Workout{
			{UserID: "U1", Activity: "跑步", DurationMin: 20, DistanceKM: 6, Calories: 314, DoneAt: now.Add(-1 * time.Hour)},
		}},
		Profiles: &stubProfiles{p: &profile.Profile{WeightKG: 60, HeightCM: 165, Age: 30, Sex: profile.SexFemale}},
	})

	turns := []struct {
		ask      string
		reEstim  bool // 要真的重新估算：回覆要有 120 以外的大卡數字，或寫出克數／每 100 克的依據
		hold     bool // 客戶沒給新資訊：不得把這一份估回紀錄值附近或改口認同紀錄
		noRecite bool // 不得只重複「去我的統計刪掉」那段
	}{
		{ask: "好市多黑麥酸種麵包 一個手掌多一點的大概多少大卡", reEstim: true},
		{ask: "查證分析清楚", reEstim: true, hold: true},
		{ask: "你分析的有問題 所以才要你重新分析", reEstim: true, hold: true},
		{ask: "好市多（Costco）科克蘭「黑麥酸麵包」一個手掌多一點的切片，熱量大約在 180 到 240 大卡之間。這款麵包每 100 公克約有 230 至 260 大卡，而手掌大小的切片重量通常落在 75 至 95 公克左右。", reEstim: true},
		{ask: "請你更新正確"},
		{ask: "你現在幫我更新", noRecite: true},
	}
	var prev string
	for i, tc := range turns {
		reply, err := a.Answer(context.Background(), "U1", tc.ask)
		if err != nil {
			t.Errorf("第 %d 句 %q 沒有回覆: %v", i+1, tc.ask, err)
			continue
		}
		t.Logf("\n[%d] 使用者：%s\n    機器人：%s", i+1, tc.ask, strings.ReplaceAll(reply.Text, "\n", "\n            "))
		if isRecordClaim(reply.Text) || isEditClaim(reply.Text) {
			t.Errorf("[%d] 過濾後仍含宣稱句", i+1)
		}
		if tc.reEstim {
			for _, d := range dodges {
				if strings.Contains(reply.Text, d) {
					t.Errorf("[%d] 拒絕重新估算：出現「%s」", i+1, d)
				}
			}
			ok := false
			for _, n := range sliceEstimates(reply.Text) {
				if n >= 140 && n <= 300 {
					ok = true
				}
			}
			if !ok {
				t.Errorf("[%d] 沒有對這一片給出 140-300 大卡的估算（排除每 100 公克參考值後：%v）", i+1, sliceEstimates(reply.Text))
			}
		}
		if tc.hold {
			if s := revertsToRecord(reply.Text); s != "" {
				t.Errorf("[%d] 沒有新資訊卻退回紀錄值附近（%d 大卡）或改口認同紀錄：%s", i+1, recordedKcal, s)
			}
		}
		if tc.noRecite && prev != "" && reply.Text == prev {
			t.Errorf("[%d] 和上一句回覆一字不差（跳針）", i+1)
		}
		prev = reply.Text
	}
}

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
