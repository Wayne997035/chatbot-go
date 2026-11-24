package webhook

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// zwj 零寬連接字(U+200D);用 rune 常數表示,原始碼裡不放看不見的字元.
const zwj = string(rune(0x200D))

// [F37-05] 第一個字素單元就超長：結果維持只有「…」（理由見 truncateLabel 註解）.
func TestButtonLabelOversizedFirstGraphemeKeepsEllipsisOnly(t *testing.T) {
	tests := []struct {
		name, in string
	}{
		{"47-unit zwj sequence then text", "👩" + strings.Repeat(zwj+"👩", 15) + "abc"},
		{"base plus 45 variation selectors", "e" + strings.Repeat("️", 45) + "x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buttonLabel(tc.in)
			if got != "…" {
				t.Errorf("buttonLabel = %q, want only the ellipsis", got)
			}
			if utf16Len(got) > maxButtonLabelLen {
				t.Errorf("utf16Len = %d, want <= %d", utf16Len(got), maxButtonLabelLen)
			}
		})
	}
}

// [F37-05] 非法 UTF-8：輸出永遠是合法 UTF-8 且不超過上限.
func TestButtonLabelInvalidUTF8IsSanitized(t *testing.T) {
	tests := []struct {
		name, in string
		limit    int
		want     string // 空字串表示只檢查性質
		prefix   string
		suffix   string
	}{
		{"short invalid run collapsed", "ab\xff\xfecd", maxButtonLabelLen, "ab�cd", "", ""},
		{"45 bad bytes collapse to one replacement", strings.Repeat("\xff", 45), maxButtonLabelLen, "�", "", ""},
		{"invalid byte inside long label", "ab\xffcd" + strings.Repeat("臺", 40), maxButtonLabelLen, "", "ab�cd", "…"},
		{"quick reply limit", "ab\xffcd", maxQuickReplyLabelLen, "ab�cd", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateLabel(tc.in, tc.limit)
			if !utf8.ValidString(got) {
				t.Errorf("output %q is not valid UTF-8", got)
			}
			if utf16Len(got) > tc.limit {
				t.Errorf("utf16Len = %d, want <= %d", utf16Len(got), tc.limit)
			}
			if tc.want != "" && got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if tc.prefix != "" && !strings.HasPrefix(got, tc.prefix) {
				t.Errorf("got %q, want prefix %q", got, tc.prefix)
			}
			if tc.suffix != "" && !strings.HasSuffix(got, tc.suffix) {
				t.Errorf("got %q, want suffix %q", got, tc.suffix)
			}
		})
	}
}
