// Package redact 移除錯誤訊息中的帳號識別碼、憑證、使用者識別碼與伺服器位址(IP、主機:埠).
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
	// keep 非 nil 時，命中字串符合 keep 就原樣保留（例如 Go 堆疊的「檔名.go:行號」）.
	keep *regexp.Regexp
}

var rules = []rule{
	// [F24-91] 連線字串帳密必須在 URL 規則之前處理，否則 userinfo 會留在 host 段.
	// [SEC-R3-03] authority 段內吃到最後一個 @：密碼含 ' 或未編碼 @、只有帳號（token）都遮；不跨 /，path 裡的 @ 不受影響.
	{pattern: regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s"]*@`), replacement: "${1}***:***@"},
	// [SEC-R3-03] 冒號形式的帳密，密碼可含未編碼 / 與 @.
	{pattern: regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s:@"]*:[^\s"]*@`), replacement: "${1}***:***@"},
	// [SEC-R3-03] 資料庫連線字串的主機（含多主機與埠）.
	{
		pattern:     regexp.MustCompile(`(?i)\b((?:mongodb(?:\+srv)?|rediss?|postgres(?:ql)?|mysql|amqps?)://(?:[^/\s"]*@)?)[^/\s"?#@]+`),
		replacement: "${1}[addr]",
	},
	// URL 只保留 scheme + host，path 與 query 可能帶 token.
	{pattern: regexp.MustCompile(`(https?://[^/\s'"]+)[^\s'"]*`), replacement: "${1}/…"},
	// [F24-91] 伺服器位址一律換成 [addr]；順序：方括號 IPv6、IPv4、主機名:埠、localhost:埠、tcp/udp、lookup、Addr:、address、x509.
	// [R3-01] 方括號內至少一個冒號才是 IPv6，slice 索引 [3] 不遮.
	{pattern: regexp.MustCompile(`\[[0-9A-Fa-f.]*:[0-9A-Fa-f:.]*(?:%[A-Za-z0-9_.-]+)?\](?::\d{1,5})?`), replacement: "[addr]"},
	{pattern: regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d{1,5})?\b`), replacement: "[addr]"},
	// [R3-01] Go 堆疊的「檔名.go:行號」不是主機:埠.
	{
		pattern:     regexp.MustCompile(`\b(?:[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}\.?:\d{1,5}\b`),
		replacement: "[addr]",
		keep:        regexp.MustCompile(`\.go:\d+$`),
	},
	{pattern: regexp.MustCompile(`\blocalhost:\d{1,5}\b`), replacement: "[addr]"},
	{pattern: regexp.MustCompile(`(?i)(\b(?:tcp|udp)[46]?\s+)[^\s:]+:\d{1,5}`), replacement: "${1}[addr]"},
	// [SEC-R3-01] Mongo SRV 查詢名帶底線（_mongodb._tcp.）.
	{pattern: regexp.MustCompile(`(?i)(\blookup\s+)[A-Za-z0-9._-]+(\s+on\s|:\s)`), replacement: "${1}[addr]${2}"},
	{pattern: regexp.MustCompile(`(?i)(\baddr(?:ess)?\s*[:=]\s*)[^\s,;)\[\]]+`), replacement: "${1}[addr]"},
	// [SEC-R3-05] net.AddrError：「address <位址>: 原因」，含不帶方括號的 IPv6.
	{pattern: regexp.MustCompile(`(?i)(\baddress\s+)\S+?(:\s)`), replacement: "${1}[addr]${2}"},
	// [SEC-R3-03] x509 主機名錯誤（crypto/x509 HostnameError 的格式）.
	{pattern: regexp.MustCompile(`(?i)(\bcannot validate certificate for\s+)\S+`), replacement: "${1}[addr]"},
	{pattern: regexp.MustCompile(`(?i)(\bbut none matched\s+)\S+`), replacement: "${1}[addr]"},
	{pattern: regexp.MustCompile(`(?i)(\bbut wanted to match\s+)\S+`), replacement: "${1}[addr]"},
	{pattern: regexp.MustCompile(`(?i)(\bcertificate is valid for\s+).*?(,\s+not\s+)\S+`), replacement: "${1}[addr]${2}[addr]"},
	// [F24-91] LINE userID 與 Workspace-ID.
	{pattern: regexp.MustCompile(`\bU[0-9a-f]{32}`), replacement: "U***"},
	{pattern: regexp.MustCompile(`\bwrkspc_[A-Za-z0-9]{8,}`), replacement: "wrkspc_***"},
	{pattern: regexp.MustCompile(`\borg_[A-Za-z0-9]{6,}`), replacement: "org_***"},
	{pattern: regexp.MustCompile(`(?i)(account\s*['"]?)[A-Za-z0-9_\-]{8,}`), replacement: "${1}***"},
	{pattern: regexp.MustCompile(`(?i)(Function\s*['"]?)[0-9a-f\-]{16,}`), replacement: "${1}***"},
	{
		pattern: regexp.MustCompile(`\b(?:AIza[0-9A-Za-z_\-]{20,}|gsk_[0-9A-Za-z]{16,}|nvapi-[0-9A-Za-z_\-]{16,}|sk-[0-9A-Za-z_\-]{16,}|` +
			`ghp_[0-9A-Za-z]{20,}|github_pat_[0-9A-Za-z_]{20,})`),
		replacement: "***",
	},
	{pattern: regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._\-]+`), replacement: "${1}***"},
	// [SEC-R3-03] Authorization: Basic（header 文字、http.Header 的 %v、JSON 陣列）.
	{
		pattern:     regexp.MustCompile(`(?i)(\bauthorization["']?\s*[:=]\s*\[?\s*["']?\s*basic\s+)[A-Za-z0-9+/=._-]+`),
		replacement: "${1}***",
	},
	// [SEC-R3-03] key 類的值：雙引號、單引號整段遮（含空白、}、跳脫引號），再處理無引號值.
	{
		pattern:     regexp.MustCompile(`(?i)((?:api[_-]?key|token|key|secret|password)["']?\s*[=:]\s*)"(?:[^"\\]|\\.)*"`),
		replacement: `${1}"***"`,
	},
	{
		pattern:     regexp.MustCompile(`(?i)((?:api[_-]?key|token|key|secret|password)["']?\s*[=:]\s*)'(?:[^'\\]|\\.)*'`),
		replacement: `${1}'***'`,
	},
	{
		pattern:     regexp.MustCompile(`(?i)((?:api[_-]?key|token|key|secret|password)["']?\s*[=:]\s*["']?)[^\s&'",;}]+`),
		replacement: "${1}***",
	},
}

// String 回傳遮蔽後的文字.
func String(s string) string {
	for _, r := range rules {
		if r.keep == nil {
			s = r.pattern.ReplaceAllString(s, r.replacement)
			continue
		}
		keep, repl := r.keep, r.replacement
		s = r.pattern.ReplaceAllStringFunc(s, func(m string) string {
			if keep.MatchString(m) {
				return m
			}
			return repl
		})
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
