package redact

import (
	"errors"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	cases := []struct {
		name, in, leak, want string
	}{
		{"groq org", "Rate limit reached in organization `org_01abcdefghijklmnop` on TPM", "org_01abcdefghij", "org_***"},
		{"nvidia account", "Function '00000000-1111-2222-3333-444444444444': Not found for account 'AbCdEfGhIjKlMn'", "AbCdEfGhIjKlMn", "account '***"},
		{"gemini key", "key AIzaSyA1234567890abcdefghijklmnop", "AIzaSyA1234567890", "***"},
		{"groq key", "invalid gsk_0123456789abcdefghij", "gsk_0123456789", "***"},
		{"nvidia key", "nvapi-0123456789abcdefghijk rejected", "nvapi-0123456789", "***"},
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
