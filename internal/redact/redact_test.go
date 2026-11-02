package redact

import (
	"errors"
	"strings"
	"testing"
)

// fakeKey 在執行時組出「長得像金鑰」的測試字串，避免原始碼裡出現會被 secret scanner 誤判的字面值.
func fakeKey(prefix string, n int) string {
	return prefix + strings.Repeat("Ab3", n)[:n]
}

func TestString(t *testing.T) {
	gemini, groq, nvidia := fakeKey("AIza", 35), fakeKey("gsk_", 24), fakeKey("nvapi-", 24)
	cases := []struct {
		name, in, leak, want string
	}{
		{"groq org", "Rate limit reached in organization `org_01abcdefghijklmnop` on TPM", "org_01abcdefghij", "org_***"},
		{"nvidia account", "Function '00000000-1111-2222-3333-444444444444': Not found for account 'AbCdEfGhIjKlMn'", "AbCdEfGhIjKlMn", "account '***"},
		{"gemini key", "key " + gemini, gemini[4:], "***"},
		{"groq key", "invalid " + groq, groq[4:], "***"},
		{"nvidia key", nvidia + " rejected", nvidia[6:], "***"},
		{"bearer", "Authorization: Bearer abc.def-ghi", "abc.def-ghi", "Bearer ***"},
		{"url", "Post https://api.line.me/v2/bot/message/12345/content?token=x", "12345", "https://api.line.me/…"},
		{"query key", "request failed key=secretvalue&x=1", "secretvalue", "key=***"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := String(tc.in)
			if strings.Contains(got, tc.leak) {
				t.Errorf("leaked %q in %q", tc.leak, got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("want %q in %q", tc.want, got)
			}
		})
	}
}

func TestError(t *testing.T) {
	if Error(nil) != "" {
		t.Error("nil error should be empty")
	}
	if got := Error(errors.New("org_abcdefghijk")); got != "org_***" {
		t.Errorf("got %q", got)
	}
}
