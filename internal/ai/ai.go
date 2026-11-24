// Package ai 提供多家 LLM 供應商（Gemini、Groq、NVIDIA）的自動選模與故障切換.
//
// 設計重點（沿用 intelligence-flow 的 Python 版本並修正其踩過的坑）：
//   - 模型不寫死：向供應商列出可用模型後依通用規則排序，舊模型下架不必改程式.
//   - 一家內逐一嘗試候選模型，整家失敗才換下一家；每家有時間預算，單次呼叫的逾時也不會超過剩餘預算.
//   - 回答必須通過 Request.Accept 檢查（例如必須是可解析的 JSON），不合格視同失敗繼續切換.
//   - 限流預設不等、直接換下一個模型（可由 RateLimitWait 設成願意原地等一次再打同一個模型）；單次請求本身超過上限或每日額度用完則直接跳過.
//   - 單次呼叫有上限（AttemptTimeout）；逾時、連不上或 5xx 的供應商冷卻一段時間，期間排到最後.
//   - 錯誤訊息一律先遮蔽帳號與金鑰（internal/redact）才寫進 log 或結果.
package ai

import (
	"context"
	"time"
)

// Capability 請求需要的模型能力.
type Capability int

const (
	// Text 純文字.
	Text Capability = iota
	// Vision 含圖片.
	Vision
)

func (c Capability) String() string {
	if c == Vision {
		return "vision"
	}
	return "text"
}

// Image 一張要給模型看的圖片.
type Image struct {
	MIME string
	Data []byte
}

// Request 一次 AI 請求.
type Request struct {
	// System 固定的系統指示（角色、規則）；可快取的部分放這裡，變動內容放 Prompt.
	System string
	Prompt string
	Images []Image
	// JSON 要求模型只輸出 JSON 物件.
	JSON bool
	// Accept 檢查回答是否可用；回傳 false 視為失敗並換下一個模型. 可為 nil.
	Accept func(text string) bool
	// Order 指定這次請求的供應商順序（例如問答優先用 Claude）；空的話依能力用預設順序.
	Order []string
	// Final 這是一則訊息的最後一步（只有問答設 true）；額度保證留 1 次給它. [F36-01]
	Final bool
}

// Capability 依內容判斷需要的模型能力.
func (r *Request) Capability() Capability {
	if len(r.Images) > 0 {
		return Vision
	}
	return Text
}

// Result 一次請求的結果.
type Result struct {
	Text     string
	Provider string
	Model    string
	// Errors 每次失敗嘗試一行簡短原因（已遮蔽），只給 log 用，不可回覆給使用者.
	Errors []string
	// Skipped 這則訊息的額度用完，沒有打任何供應商. [F36-01]
	Skipped bool
	// Attempts 實際打了幾個模型（一次 p.Call 算一個）. [F36-02]
	Attempts int
	// ImageRejected 照片是結構壞掉的 JPEG，剝除中繼資料失敗所以沒有送出（Attempts 為 0、不扣額度）；
	// 重試同一張永遠不會成功，呼叫端應請使用者換一張. [F37-34]
	ImageRejected bool
}

// OK 是否取得可用回答.
func (r *Result) OK() bool { return r.Text != "" }

// Provider 一家 AI 供應商.
type Provider interface {
	// Name 供應商代號（gemini / groq / nvidia）.
	Name() string
	// Candidates 依能力回傳排序後的候選模型，最適合的在前.
	Candidates(ctx context.Context, c Capability) ([]string, error)
	// Call 用指定模型完成請求.
	Call(ctx context.Context, model string, req *Request) (string, error)
}

// Settings 路由設定；零值欄位會套用預設.
type Settings struct {
	// TextOrder 純文字請求的供應商順序.
	TextOrder []string
	// VisionOrder 含圖片請求的供應商順序.
	VisionOrder []string
	// ProviderBudget 每家供應商每次請求的時間上限.
	ProviderBudget time.Duration
	// MaxAttempts 每家供應商每次請求最多嘗試的模型數.
	MaxAttempts int
	// MaxTotalAttempts 一次 Router.Complete 跨供應商合計最多嘗試的模型數（預設 3）. [F36-02]
	MaxTotalAttempts int
	// RateLimitWait 每分鐘限流時，願意原地等待再重試同模型的最長時間；0（預設）= 不等、直接換模型，負值視為 0.
	RateLimitWait time.Duration
	// CandidateTTL 模型清單快取時間.
	CandidateTTL time.Duration
	// AttemptTimeout 單次模型呼叫的時間上限（不得超過這家剩下的預算與呼叫端期限）.
	AttemptTimeout time.Duration
	// ProviderCooldown 供應商逾時、連不上或 5xx 後，後續請求把它排到最後的時間.
	ProviderCooldown time.Duration
	// Models 每家指定的候選模型清單（key 為 ProviderNVIDIA / ProviderGroq / ProviderGemini），依序嘗試；
	// nil、缺 key 或空切片 = 該家用自動排名；Claude 的 key 不採用.
	Models map[string][]string
}

// 預設值：聊天機器人要在 LINE reply token 失效前回覆，所以比排程報告短很多.
const (
	defaultProviderBudget   = 10 * time.Second
	defaultAttemptTimeout   = 8 * time.Second
	defaultProviderCooldown = 60 * time.Second
	defaultMaxAttempts      = 4
	defaultMaxTotalAttempts = 3
	defaultCandidateTTL     = 6 * time.Hour
)

// 供應商代號；設定的順序（AI_*_PROVIDER_ORDER）用的就是這些名稱.
const (
	ProviderGemini = "gemini"
	ProviderGroq   = "groq"
	ProviderNVIDIA = "nvidia"
	ProviderClaude = "claude"
)

// 預設順序只含免費供應商；Claude（付費）只在請求明確指定 Order 時使用.
var (
	defaultTextOrder   = []string{ProviderGroq, ProviderGemini, ProviderNVIDIA}
	defaultVisionOrder = []string{ProviderGemini, ProviderGroq, ProviderNVIDIA}
)

func (s *Settings) withDefaults() Settings {
	out := *s
	if len(out.TextOrder) == 0 {
		out.TextOrder = defaultTextOrder
	}
	if len(out.VisionOrder) == 0 {
		out.VisionOrder = defaultVisionOrder
	}
	if out.ProviderBudget <= 0 {
		out.ProviderBudget = defaultProviderBudget
	}
	if out.MaxAttempts <= 0 {
		out.MaxAttempts = defaultMaxAttempts
	}
	if out.MaxTotalAttempts <= 0 {
		out.MaxTotalAttempts = defaultMaxTotalAttempts
	}
	// [F33-05] 預設就是 0（不等）；負值夾成 0，0 不再被當成「未設」而改回 8s.
	if out.RateLimitWait < 0 {
		out.RateLimitWait = 0
	}
	// [F33-06] 單次與冷卻的預設值；呼叫端沒設環境變數也要有快速切換.
	if out.AttemptTimeout <= 0 {
		out.AttemptTimeout = defaultAttemptTimeout
	}
	if out.ProviderCooldown <= 0 {
		out.ProviderCooldown = defaultProviderCooldown
	}
	// [F33-01] 深拷貝並丟掉 Claude 的 key：呼叫端事後改動不影響路由，Claude 只用建構子指定的模型.
	out.Models = cloneModels(out.Models)
	if out.CandidateTTL <= 0 {
		out.CandidateTTL = defaultCandidateTTL
	}
	return out
}

// cloneModels 深拷貝每家的模型清單：丟掉空清單與 Claude 的 key，同一清單內重複的模型只留第一個.
func cloneModels(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for name, models := range in {
		if name == ProviderClaude || len(models) == 0 {
			continue
		}
		seen := make(map[string]bool, len(models))
		list := make([]string, 0, len(models))
		for _, m := range models {
			if !seen[m] {
				seen[m] = true
				list = append(list, m)
			}
		}
		out[name] = list
	}
	return out
}

// limitsProvider 自帶單次與總時限的供應商不套 Settings.AttemptTimeout / ProviderBudget（目前只有 Claude）.
type limitsProvider interface {
	Limits() (attempt, budget time.Duration)
}

// visionChecker 能依模型名判斷是否能看圖的供應商；清單模式看圖請求用它過濾.
type visionChecker interface {
	SupportsVision(model string) bool
}
