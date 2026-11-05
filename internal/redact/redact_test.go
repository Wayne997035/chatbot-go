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

// fakeUserID 組出 LINE userID 形狀的假值：U 加 32 個十六進位字元.
const fakeUserID = "Uaaaaaaaabbbbbbbbccccccccdddddddd"

// addressCases 是 [F24-91] 伺服器位址表；位址只用內網與測試網段.
var addressCases = []struct {
	name, in, leak string
}{
	{"dial tcp ipv4 port", "dial tcp 10.0.0.5:6379: connect: connection refused", "10.0.0.5"},
	{"cloud host port", "connect to cache-1.region.example.net:18443 failed", "cache-1.region"},
	{"ipv6 loopback port", "dial tcp [::1]:6379: connect: connection refused", "[::1]"},
	{"ipv6 zone port", "dial tcp [fe80::1%eth0]:6379: refused", "fe80"},
	{"fqdn trailing dot", "connect to redis.example.com.:6379 failed", "redis.example.com"},
	{"localhost port", "connect to localhost:27017 failed", "localhost:27017"},
	{"ipv4 only", "upstream 192.0.2.7 timed out", "192.0.2.7"},
	{"url with ipv4 port path", "Get http://10.0.0.9:8080/internal/health: EOF", "10.0.0.9"},
	{"single label tcp", "dial tcp redis:6379: connect: connection refused", "redis:6379"},
	{"lookup", "dial tcp: lookup redis on 10.0.0.2:53: no such host", "lookup redis"},
	{"Addr keyword", "server selection error: Addr: mongo:27017", "mongo:27017"},
	// [SEC-R3-01] Mongo SRV 查詢名帶底線.
	{"lookup srv on resolver", "dial tcp: lookup _mongodb._tcp.cluster0.abcde.mongodb.net on 10.0.0.1:53: no such host", "cluster0"},
	{"lookup srv", "lookup _mongodb._tcp.cluster0.abcde.mongodb.net: no such host", "cluster0"},
	// [SEC-R3-05] net.AddrError 的不帶方括號 IPv6.
	{"addr error ipv6", "dial tcp: address fd00::1:6379: too many colons in address", "fd00"},
	// [SEC-R3-03] x509 主機名錯誤.
	{"x509 valid for not", "x509: certificate is valid for a.internal.example, b.internal.example, not db.internal.example", "internal.example"},
	{"x509 wanted to match", "x509: certificate is not valid for any names, but wanted to match db.internal.example", "db.internal"},
	{"x509 none matched", "x509: certificate is valid for 2 names, but none matched db.internal.example", "db.internal"},
	{"x509 ip sans", "x509: cannot validate certificate for fd00::7 because it doesn't contain any IP SANs", "fd00"},
	// [SEC-R3-03] 資料庫連線字串的主機.
	{"mongodb srv host", "mongodb+srv://cluster0.abcde.mongodb.net/db", "cluster0"},
	{"mongodb multi host", "mongodb://h1.example.net,h2.example.net/db?x=1", "h2.example"},
}

func TestStringMasksServerAddresses(t *testing.T) {
	for _, tc := range addressCases {
		t.Run(tc.name, func(t *testing.T) {
			got := String(tc.in)
			if strings.Contains(got, tc.leak) {
				t.Errorf("leaked %q in %q", tc.leak, got)
			}
			if !strings.Contains(got, "[addr]") {
				t.Errorf("want [addr] in %q", got)
			}
		})
	}
	if got := String("Get http://10.0.0.9:8080/internal/health: EOF"); !strings.Contains(got, "http://[addr]/…") {
		t.Errorf("want http://[addr]/… in %q", got)
	}
	if got := String("mongodb+srv://cluster0.abcde.mongodb.net/db"); !strings.Contains(got, "mongodb+srv://[addr]/db") {
		t.Errorf("want mongodb+srv://[addr]/db in %q", got)
	}
}

// credCases 是 [F24-91] 連線字串帳密與識別碼表.
var credCases = []struct {
	name, in, leak, want string
}{
	{"redis empty user", "dial redis://:fakepass1@10.0.0.5:6379/0 failed", "fakepass1", "redis://***:***@"},
	{"mongodb srv", "mongodb+srv://fakeuser:fakepass2@cluster.example.net/db", "fakepass2", "mongodb+srv://***:***@"},
	{"mongodb srv user", "mongodb+srv://fakeuser:fakepass2@cluster.example.net/db", "fakeuser", "mongodb+srv://"},
	{"rediss default", "rediss://default:fakepass3@cache.example.net:6380", "fakepass3", "rediss://***:***@"},
	{"https userinfo", "https://fakeuser:fakepass4@api.example.com/v1", "fakepass4", "https://***:***@"},
	{"https userinfo user", "https://fakeuser:fakepass4@api.example.com/v1", "fakeuser", "https://"},
	{"line userid suffix", fakeUserID + "_earthquake_tsunami", fakeUserID[1:], "U***_earthquake_tsunami"},
	{"line userid in dup key", `dup key: { _id: "` + fakeUserID + `" }`, fakeUserID[1:], "U***"},
	{"workspace id", "workspace wrkspc_Abcdefgh12345 not found", "Abcdefgh12345", "wrkspc_***"},
	// [SEC-R3-03] userinfo 的各種寫法；假帳密在執行時組出，理由同 fakeKey.
	{"password with quote", "mongodb://admin:" + "pa'ss" + "@cluster0.example.net:27017/db", "pa'ss", "***:***@"},
	{"token as user", "https://" + fakeToken + "@api.example.com/", fakeToken, "***:***@"},
	{"password with at", "redis://u:" + "p@ss" + "@cache.example.net:6379", "ss@", "***:***@"},
	{"password with slash", "mongodb://user:" + "pa/ss" + "@host.example.net/db", "ss@host", "***:***@"},
	{"password with slash and at", "mongodb://user:" + "p/a@ss" + "@host.example.net/db", "ss@host", "***:***@"},
	// [SEC-R3-03] JSON 與引號寫法的值、Authorization: Basic.
	{"json password", `{"password":"` + "fakepass5" + `"}`, "fakepass5", "***"},
	{"json password with spaces", `{"password":"correct horse battery"}`, "horse", "***"},
	{"json secret with brace", `{"secret":"a}b"}`, "a}b", "***"},
	{"json password escaped quote", `{"password":"pa\"ss word"}`, "ss word", "***"},
	{"single quoted password", `password='two words'`, "words", "***"},
	{"json password number", `{"password":12345}`, "12345", "***"},
	{"json token bare", `{"token":abc123}`, "abc123", "***"},
	{"basic header", "Authorization: Basic " + fakeBasic, fakeBasic[:12], "***"},
	{"basic in header map", "map[Authorization:[Basic " + fakeBasic + "]]", fakeBasic[:12], "***"},
	{"basic in json array", `{"Authorization": ["Basic ` + fakeBasic + `"]}`, fakeBasic[:12], "***"},
}

// fakeToken、fakeBasic 是 [SEC-R3-03] 的假憑證：只當遮蔽測試的輸入.
var (
	fakeToken = "tokenvalue" + "123456"
	fakeBasic = "ZmFrZTpmYWtl" + "cGFzcw=="
)

func TestStringMasksIdentifiersAndCredentials(t *testing.T) {
	for _, tc := range credCases {
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

func TestStringKeepsOrdinaryText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"clock", "now 12:30:45", "12:30:45"},
		{"chinese place", "查詢 信義區 天氣", "信義區"},
		{"line api url", "https://api.line.me/v2/bot/message/push", "https://api.line.me/…"},
		{"ratio", "ratio 3:1", "ratio 3:1"},
		{"key colon number", "key:12", "key:"},
		{"retry", "retry 2:30", "retry 2:30"},
		{"ubuntu", "Ubuntu 12:30", "Ubuntu 12:30"},
		// [R3-01] Go 堆疊的檔名行號與 slice 索引不是位址.
		{"go stack frame", "/app/internal/webhook/router.go:82 +0x1a4", "router.go:82"},
		{"slice index", "index out of range [3] with length 2", "[3]"},
		// [SEC-R3-03] path 裡的 @ 不是 userinfo；一般文字裡的 Basic、token 不遮.
		{"at in path", "https://example.com/users/@john", "https://example.com/…"},
		{"basic auth words", "Basic authentication failed", "authentication"},
		{"token words", "token expired", "token expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := String(tc.in)
			for _, bad := range []string{"[addr]", "***:***@", "U***"} {
				if strings.Contains(got, bad) {
					t.Errorf("unexpected %q in %q", bad, got)
				}
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("want %q in %q", tc.want, got)
			}
		})
	}
}

func TestStringIsIdempotent(t *testing.T) {
	var inputs []string
	for _, tc := range addressCases {
		inputs = append(inputs, tc.in)
	}
	for _, tc := range credCases {
		inputs = append(inputs, tc.in)
	}
	for _, in := range inputs {
		once := String(in)
		if twice := String(once); twice != once {
			t.Errorf("not idempotent: %q -> %q -> %q", in, once, twice)
		}
	}
}
