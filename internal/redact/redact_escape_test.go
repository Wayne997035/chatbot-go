package redact

import (
	"fmt"
	"strings"
	"testing"
	"time"
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

// [F37-32] 未結束的引號（被長度上限截斷）仍要遮住憑證，並一路遮到字串結尾.
func TestStringMasksUnterminated(t *testing.T) {
	tests := []struct {
		name string
		in   string
		leak string
		want string
	}{
		{"double_quote_key", `token="abc123SECRETxyz and more text`, "SECRETxyz", `token="***"`},
		{"double_quote_space", `password: "hunter2 SECRETtail`, "SECRETtail", `password: "***"`},
		{"single_quote_key", `api_key='SK123SECRET unterminated`, "SK123SECRET", `api_key='***'`},
		{"invalid_host", `invalid host "h:1SECRET unterminated`, "SECRET", `invalid host "[addr]"`},
		{"invalid_option_value", `invalid option value "p@ssSECRET`, "SECRET", `invalid option value "***"`},
		{"trailing_single_backslash", "token=\"abc\\", "abc", `token="***"`},
		{"real_percent_q", `invalid option value ` + fmt.Sprintf("%q", "p\"wSECRET\nd"), "SECRET", `invalid option value "***"`},
		{"json_body", `{"error":"bad","token":"abcSECRET\"tail`, "SECRET", `{"error":"bad","token":"***"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := String(tt.in)
			if strings.Contains(out, tt.leak) || strings.Contains(out, "more text") {
				t.Fatalf("leaks %q: %q", tt.leak, out)
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

// [F37-32] 引號內「反斜線 + 真換行」不得讓遮蔽提早結束.
func TestStringMasksBackslashNewline(t *testing.T) {
	tests := []struct{ name, in, leak, want string }{
		{"json", "{\"token\":\"abc\\\nSECRETtail\"}", "SECRETtail", `{"token":"***"}`},
		{"kv_then_text", "token=\"abc\\\nSECRETtail\" end", "SECRETtail", `token="***" end`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := String(tt.in)
			if strings.Contains(out, tt.leak) || out != tt.want {
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

// [F37-32] 已閉合的值仍各自遮蔽，中間文字保留.
func TestStringClosedValuesStillSeparate(t *testing.T) {
	in := `token="ok" then password="x"`
	if got, want := String(in), `token="***" then password="***"`; got != want {
		t.Fatalf("String(%q) = %q, want %q", in, got, want)
	}
}

// [F37-32] 100 KB 的引號與反斜線不得造成災難性比對.
func TestStringLargeQuoteInputIsFast(t *testing.T) {
	in := `token="` + strings.Repeat(`"\`, 50*1024)
	start := time.Now()
	_ = String(in)
	if d, limit := time.Since(start), time.Second*raceSlowdown; d > limit {
		t.Fatalf("String took %v on 100KB input, want < %v", d, limit)
	}
}
