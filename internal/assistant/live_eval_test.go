//go:build liveeval

package assistant

// 真模型評測：照正式環境的供應商設定呼叫真的 AI，重播客戶實際對話.
// 平常的 go test 與 CI 不會編進來；手動執行（金鑰放在 configs/local.yaml 的 ai 區段，和 task run 同一份）：
//   APP_ENV=local go test -tags liveeval ./internal/assistant/ -run TestLive -v -count=1
// 金鑰只在程式內使用，不印出；讀不到設定或沒有金鑰直接失敗.

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

var kcalRe = regexp.MustCompile(`(\d{2,4})\s*(?:[-–~到至]\s*(\d{2,4}))?\s*(?:大卡|kcal)`)

// kcalNumbers 回覆裡出現的所有大卡數字（範圍兩端都算）.
func kcalNumbers(s string) []int {
	var out []int
	for _, m := range kcalRe.FindAllStringSubmatch(s, -1) {
		for _, g := range m[1:] {
			if n, err := strconv.Atoi(g); err == nil {
				out = append(out, n)
			}
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
		noRecite bool // 不得只重複「去我的統計刪掉」那段
	}{
		{ask: "好市多黑麥酸種麵包 一個手掌多一點的大概多少大卡", reEstim: true},
		{ask: "查證分析清楚", reEstim: true},
		{ask: "你分析的有問題 所以才要你重新分析", reEstim: true},
		{ask: "好市多（Costco）科克蘭「黑麥酸麵包」一個手掌多一點的切片，熱量大約在 180 到 240 大卡之間。這款麵包每 100 公克約有 230 至 260 大卡，而手掌大小的切片重量通常落在 75 至 95 公克左右。", reEstim: true},
		{ask: "請你更新正確"},
		{ask: "你現在幫我更新", noRecite: true},
	}
	var prev string
	for i, tc := range turns {
		reply, err := a.Answer(context.Background(), "U1", tc.ask)
		if err != nil {
			t.Fatalf("第 %d 句 %q: %v", i+1, tc.ask, err)
		}
		t.Logf("\n[%d] 使用者：%s\n    機器人：%s", i+1, tc.ask, strings.ReplaceAll(reply.Text, "\n", "\n            "))
		if isRecordClaim(reply.Text) || isEditClaim(reply.Text) {
			t.Errorf("[%d] 過濾後仍含宣稱句", i+1)
		}
		if tc.reEstim {
			nums := kcalNumbers(reply.Text)
			other := false
			for _, n := range nums {
				if n != 120 && n != 620 && n != 500 && n != 314 {
					other = true
				}
			}
			if !other && !strings.Contains(reply.Text, "克") {
				t.Errorf("[%d] 沒有重新估算：大卡數字 %v、也沒有寫克數依據", i+1, nums)
			}
		}
		if tc.noRecite && prev != "" && reply.Text == prev {
			t.Errorf("[%d] 和上一句回覆一字不差（跳針）", i+1)
		}
		prev = reply.Text
	}
}

// liveAIConfig 照正式程式的路徑讀設定（APP_ENV + configs/<env>.yaml + keyset 解密），金鑰只放這一個地方；
// 刻意跑真模型卻讀不到設定或沒有金鑰時直接失敗，不准 SKIP 成假綠燈.
func liveAIConfig(t *testing.T) *config.AIConfig {
	t.Helper()
	if os.Getenv("APP_ENV") == "" {
		t.Fatal("要設 APP_ENV（本機用 local），和 task run 讀同一份設定")
	}
	chdirToConfigRoot(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("讀設定失敗: %v", err)
	}
	c := &cfg.AI
	if c.GeminiAPIKey == "" && c.GroqAPIKey == "" && c.NVIDIAAPIKey == "" && c.AnthropicAPIKey == "" {
		t.Fatal("設定裡沒有任何 AI 金鑰（ai.anthropic_api_key / gemini_api_key / groq_api_key / nvidia_api_key）")
	}
	return c
}

// chdirToConfigRoot 往上找第一個有 configs/<APP_ENV>.yaml 的目錄並切過去（設定檔用相對路徑讀，worktree 裡沒有本機設定）.
func chdirToConfigRoot(t *testing.T) {
	t.Helper()
	env := os.Getenv("APP_ENV")
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取不到工作目錄: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "configs", env+".yaml")); err == nil {
			t.Chdir(dir)
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("往上找不到 configs/%s.yaml", env)
		}
		dir = parent
	}
}
