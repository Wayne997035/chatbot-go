// Package redact 移除錯誤訊息中的帳號識別碼與憑證.
//
// AI 供應商與 HTTP 錯誤常夾帶組織 ID、帳號 ID、API key 或完整 URL，
// 任何要寫進 log 或回覆給使用者的錯誤字串都必須先經過 String().
package redact

import (
	"fmt"
	"regexp"
)

type rule struct {
	pattern     *regexp.Regexp
	replacement string
}

var rules = []rule{
	// URL 只保留 scheme + host，path 與 query 可能帶 token.
	{regexp.MustCompile(`(https?://[^/\s'"]+)[^\s'"]*`), "${1}/…"},
	{regexp.MustCompile(`\borg_[A-Za-z0-9]{6,}`), "org_***"},
	{regexp.MustCompile(`(?i)(account\s*['"]?)[A-Za-z0-9_\-]{8,}`), "${1}***"},
	{regexp.MustCompile(`(?i)(Function\s*['"]?)[0-9a-f\-]{16,}`), "${1}***"},
	{
		regexp.MustCompile(`\b(?:AIza[0-9A-Za-z_\-]{20,}|gsk_[0-9A-Za-z]{16,}|nvapi-[0-9A-Za-z_\-]{16,}|sk-[0-9A-Za-z_\-]{16,}|` +
			`ghp_[0-9A-Za-z]{20,}|github_pat_[0-9A-Za-z_]{20,})`),
		"***",
	},
	{regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._\-]+`), "${1}***"},
	{regexp.MustCompile(`(?i)((?:api[_-]?key|token|key|secret|password)\s*[=:]\s*)[^\s&'",;]+`), "${1}***"},
}

// String 回傳遮蔽後的文字.
func String(s string) string {
	for _, r := range rules {
		s = r.pattern.ReplaceAllString(s, r.replacement)
	}
	return s
}

// Error 回傳遮蔽後的錯誤訊息；err 為 nil 時回傳空字串.
func Error(err error) string {
	if err == nil {
		return ""
	}
	return String(fmt.Sprint(err))
}
