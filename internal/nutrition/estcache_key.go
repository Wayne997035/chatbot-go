package nutrition

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// [F37-41] 記餐文字估算快取的 key 組裝.
//
// key = mealest:v<schema>:<promptHash8>:<c|n>:<sha256hex(NormalizeMealText(text))>
// 不含 userID（EstimateText 也沒有 userID 參數）；key 與文字雜湊都不進日誌.

const (
	// estimateSchema 快取值的 schema 版本；值的形狀或 parseTextEstimate/cleanFood 行為改變時加一，舊 key 自然作廢.
	estimateSchema = 1
	// estimateKeyPrefix Redis key 前綴（測試與 grep 用）.
	estimateKeyPrefix = "mealest:v"
)

// estimatePromptHash 兩種模式（可追問 / 不追問）的 prompt 雜湊前 8 個十六進位字；prompt 一改 key 就變.
// 測試會換掉這兩個值來驗證 key 跟著 prompt 變.
var estimatePromptHash = [2]string{
	promptHash8(textPrompt),
	promptHash8(textPrompt + noClarifyRule),
}

func promptHash8(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])[:8]
}

// estimateKey 組 key；clarify 為 allowClarify（c）或不追問（n）.
func estimateKey(schema int, promptHash string, allowClarify bool, text string) string {
	mode := "n"
	if allowClarify {
		mode = "c"
	}
	sum := sha256.Sum256([]byte(NormalizeMealText(text)))
	return estimateKeyPrefix + strconv.Itoa(schema) + ":" + promptHash + ":" + mode + ":" + hex.EncodeToString(sum[:])
}

// currentEstimateKey 以目前的 schema 與 prompt 雜湊組 key.
func currentEstimateKey(allowClarify bool, text string) string {
	idx := 1
	if allowClarify {
		idx = 0
	}
	return estimateKey(estimateSchema, estimatePromptHash[idx], allowClarify, text)
}
