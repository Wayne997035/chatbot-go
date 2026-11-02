// Package ai 提供多家 LLM 供應商（Gemini、Groq、NVIDIA）的自動選模與故障切換.
//
// 設計重點（沿用 intelligence-flow 的 Python 版本並修正其踩過的坑）：
//   - 模型不寫死：向供應商列出可用模型後依通用規則排序，舊模型下架不必改程式.
//   - 一家內逐一嘗試候選模型，整家失敗才換下一家；每家有時間預算，單次呼叫的逾時也不會超過剩餘預算.
//   - 回答必須通過 Request.Accept 檢查（例如必須是可解析的 JSON），不合格視同失敗繼續切換.
//   - 每分鐘限流且等待時間短時，先等一次再打同一個模型；單次請求本身超過上限或每日額度用完則直接跳過.
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
	// RateLimitWait 每分鐘限流時，願意原地等待再重試同模型的最長時間.
	RateLimitWait time.Duration
	// CandidateTTL 模型清單快取時間.
	CandidateTTL time.Duration
}

// 預設值：聊天機器人要在 LINE reply token 失效前回覆，所以比排程報告短很多.
const (
	defaultProviderBudget = 25 * time.Second
	defaultMaxAttempts    = 4
	defaultRateLimitWait  = 8 * time.Second
	defaultCandidateTTL   = 6 * time.Hour
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
	if out.RateLimitWait <= 0 {
		out.RateLimitWait = defaultRateLimitWait
	}
	if out.CandidateTTL <= 0 {
		out.CandidateTTL = defaultCandidateTTL
	}
	return out
}
