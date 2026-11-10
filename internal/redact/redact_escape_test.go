package redact

import (
	"strings"
	"testing"
)

// [F291-10] 引號內含跳脫引號（\"）時，尾段不得外洩. 輸入照上游真實格式：
// mongo-driver connstring.go 的 invalid host %q / invalid option key %q / invalid option value %q、Go net/url 的 strconv.Quote.
func TestStringMasksEscapedQuoteTails(t *testing.T) {
	tests := []struct {
		name string
		in   string
		tail string
		want string
	}{
		{"invalid_host", `invalid host "h\"fakeHostTail:1"`, "fakeHostTail", `invalid host "[addr]"`},
		{"invalid_host_url_prefix", `invalid host "https://h\"fakeHostUrlTail"`, "fakeHostUrlTail", `invalid host "[addr]"`},
		{"invalid_url_escape", `parse "x": invalid URL escape "%\"fakeEscTail"`, "fakeEscTail", `parse "x": invalid URL escape "***"`},
		{"invalid_url_escape_url_prefix", `invalid URL escape "https://h\"fakeEscUrlTail%zz"`, "fakeEscUrlTail", `invalid URL escape "***"`},
		{"hostname", `unexpected hostname "h\"fakeHnTail"`, "fakeHnTail", `unexpected hostname "[addr]"`},
		{"hostname_url_prefix", `unexpected hostname "https://h\"fakeHnUrlTail"`, "fakeHnUrlTail", `unexpected hostname "[addr]"`},
		{"invalid_option_value", `invalid option value "https://h\"fakeOptTail%zz"`, "fakeOptTail", `invalid option value "***"`},
		{"invalid_option_key", `invalid option key "https://h\"fakeKeyTail"`, "fakeKeyTail", `invalid option key "***"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := String(tt.in)
			if strings.Contains(out, tt.tail) {
				t.Fatalf("tail %q leaked: %q", tt.tail, out)
			}
			if out != tt.want {
				t.Fatalf("String(%q) = %q, want %q", tt.in, out, tt.want)
			}
		})
		t.Run(tt.name+"/idempotent", func(t *testing.T) {
			once := String(tt.in)
			if twice := String(once); twice != once {
				t.Fatalf("not idempotent: %q then %q", once, twice)
			}
		})
	}
}

// 沒有跳脫引號的輸入，引號後面的文字不得被多遮.
func TestStringEscapeAwareRulesDoNotOverMask(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"invalid_host", `invalid host "bad:1" then code 5`, `invalid host "[addr]" then code 5`},
		{"invalid_url_escape", `invalid URL escape "%zz" then code 5`, `invalid URL escape "***" then code 5`},
		{"hostname", `unexpected hostname "h" then code 5`, `unexpected hostname "[addr]" then code 5`},
		{"invalid_option_key", `invalid option key "k" then code 5`, `invalid option key "***" then code 5`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := String(tt.in); got != tt.want {
				t.Fatalf("String(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
