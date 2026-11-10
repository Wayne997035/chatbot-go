package logger

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gopkg.in/natefinch/lumberjack.v2"
)

const testAddr = "10.0.0.5:6379"

func dialErr() error {
	return errors.New("dial tcp " + testAddr + ": connect: connection refused")
}

func restoreDefault(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func parseLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func asString(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("%s is %T (%v), want string", key, m[key], m[key])
	}
	return s
}

func wantMasked(t *testing.T, name, got string) {
	t.Helper()
	if strings.Contains(got, "10.0.0.5") {
		t.Errorf("%s leaked ip: %q", name, got)
	}
	if !strings.Contains(got, "[addr]") {
		t.Errorf("%s missing [addr]: %q", name, got)
	}
}

// F24-92: 沒包 redact 的屬性寫出去時已遮.
func TestHandlerRedactsAttributes(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(newHandler(&buf, slog.LevelInfo))
	l.Warn(
		"dial tcp "+testAddr+" failed",
		"error", dialErr(),
		"cause", stackErr{},
		"addr", "redis:6379",
		slog.Group("redis", "error", dialErr()),
		slog.Any("hosts", []string{testAddr}),
		"district", "信義區",
		"count", 3,
		"host", "",
	)

	lines := parseLines(t, buf.Bytes())
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	m := lines[0]
	wantMasked(t, "msg", asString(t, m, "msg"))
	wantMasked(t, "error", asString(t, m, "error"))
	// cause 走 error 分支(Error())；若落到非字串分支會用 %+v 展開而帶出 STACK-DETAIL.
	if got := asString(t, m, "cause"); got != "dial tcp [addr]: connection refused" {
		t.Errorf("cause = %q, want %q", got, "dial tcp [addr]: connection refused")
	}
	grp, ok := m["redis"].(map[string]any)
	if !ok {
		t.Fatalf("redis group is %T", m["redis"])
	}
	wantMasked(t, "redis.error", asString(t, grp, "error"))
	if got := asString(t, m, "addr"); got != "[addr]" {
		t.Errorf("addr = %q, want [addr]", got)
	}
	wantMasked(t, "hosts", asString(t, m, "hosts"))
	if got := asString(t, m, "district"); got != "信義區" {
		t.Errorf("district = %q", got)
	}
	if got, ok := m["count"].(float64); !ok || got != 3 {
		t.Errorf("count = %v (%T), want number 3", m["count"], m["count"])
	}
	// [T3-02] source 仍是物件；空的位址屬性不改成 [addr].
	src, ok := m["source"].(map[string]any)
	if !ok {
		t.Fatalf("source is %T, want an object", m["source"])
	}
	for _, k := range []string{"function", "file", "line"} {
		if _, ok := src[k]; !ok {
			t.Errorf("source missing %q: %v", k, src)
		}
	}
	if got := asString(t, m, "host"); got != "" {
		t.Errorf("empty host = %q, want empty", got)
	}
}

// 假 LINE userID：U 加 32 個十六進位字元.
const fakeLineUserID = "Uaaaaaaaabbbbbbbbccccccccdddddddd"

type hostHolder struct{ Host string }

type addrPointer struct{ Inner *hostHolder }

// stringerHidesAddr 的 String() 無害，但 JSON 會寫出欄位裡的位址.
type stringerHidesAddr struct{ Host string }

func (stringerHidesAddr) String() string { return "harmless" }

type credFields struct{ Token, Password, APIKey string }

type renamedToken struct {
	Token string `json:"t"`
}

// [SEC-R3-02] 寫出的必須是檢查過的那一份：%+v 與 JSON 兩份都檢查.
func TestRedactAttrChecksWhatIsWritten(t *testing.T) {
	cases := []struct {
		name     string
		attr     slog.Attr
		leaks    []string
		wantAddr bool
	}{
		{"raw json", slog.Any("v", json.RawMessage(`{"host":"`+testAddr+`"}`)), []string{"10.0.0.5"}, true},
		{"bytes", slog.Any("v", []byte("dial tcp "+testAddr)), []string{"10.0.0.5"}, true},
		{"nested pointer", slog.Any("v", addrPointer{Inner: &hostHolder{Host: testAddr}}), []string{"10.0.0.5"}, false},
		{"stringer hides address", slog.Any("v", stringerHidesAddr{Host: testAddr}), []string{"10.0.0.5"}, false},
		{"key with user id", slog.String(fakeLineUserID, "x"), []string{fakeLineUserID[1:]}, false},
		{
			"credential fields",
			slog.Any("v", credFields{Token: "tok-fake-1", Password: "pw-fake-1", APIKey: "key-fake-1"}),
			[]string{"tok-fake-1", "pw-fake-1", "key-fake-1"},
			false,
		},
		{"map token", slog.Any("v", map[string]string{"token": "tok-fake-2"}), []string{"tok-fake-2"}, false},
		{"renamed json tag", slog.Any("v", renamedToken{Token: "tok-fake-3"}), []string{"tok-fake-3"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			slog.New(newHandler(&buf, slog.LevelInfo)).LogAttrs(context.Background(), slog.LevelInfo, "m", tc.attr)
			line := buf.String()
			for _, l := range tc.leaks {
				if strings.Contains(line, l) {
					t.Errorf("leaked %q: %s", l, line)
				}
			}
			if tc.wantAddr && !strings.Contains(line, "[addr]") {
				t.Errorf("want [addr] in %s", line)
			}
		})
	}
}

type bytesField struct{ Body []byte }

type namedBytes []byte

// [SEC-R3-02] 已知限制，不是保證：巢狀與具名 []byte 被 json 編碼成 base64，redact 規則看不穿（修改單 B P3 第 4 點）.
// 這支測試釘住目前行為；有人新增這種 log 呼叫點時，讀得到這條限制.
func TestRedactAttrNestedBytesIsKnownGap(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("dial tcp " + testAddr))
	for _, tc := range []struct {
		name string
		v    any
	}{
		{"struct field", bytesField{Body: []byte("dial tcp " + testAddr)}},
		{"named type", namedBytes("dial tcp " + testAddr)},
	} {
		var buf bytes.Buffer
		slog.New(newHandler(&buf, slog.LevelInfo)).Info("m", slog.Any("v", tc.v))
		if !strings.Contains(buf.String(), enc) {
			t.Errorf("%s: expected the known base64 gap, got %s", tc.name, buf.String())
		}
	}
}

// [T3-02] source 物件原樣保留，即使檔案路徑剛好長得像位址.
func TestRedactAttrKeepsSource(t *testing.T) {
	src := &slog.Source{Function: "f", File: "/srv/10.0.0.5/app/x.go", Line: 3}
	got := redactAttr(nil, slog.Any(slog.SourceKey, src))
	s, ok := got.Value.Any().(*slog.Source)
	if !ok || s != src || s.File != "/srv/10.0.0.5/app/x.go" {
		t.Errorf("source changed: %v", got)
	}
}

// [SEC-R3-02] Init 之前（slog 預設 handler，不經出口遮蔽）開機致命錯誤也是 ERROR 且已遮蔽.
func TestReportFatalBeforeInit(t *testing.T) {
	restoreDefault(t)
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	ReportFatal(dialErr())
	lines := parseLines(t, buf.Bytes())
	if len(lines) != 1 || lines[0]["level"] != "ERROR" {
		t.Fatalf("got %v, want one ERROR record", lines)
	}
	if strings.Contains(buf.String(), "10.0.0.5") {
		t.Errorf("leaked address: %s", buf.String())
	}
}

// [SEC-R3-02] Init 之後同樣是 ERROR 且已遮蔽.
func TestReportFatalAfterInit(t *testing.T) {
	restoreDefault(t)
	var buf bytes.Buffer
	slog.SetDefault(slog.New(newHandler(&buf, slog.LevelInfo)))
	ReportFatal(dialErr())
	lines := parseLines(t, buf.Bytes())
	if len(lines) != 1 || lines[0]["level"] != "ERROR" {
		t.Fatalf("got %v, want one ERROR record", lines)
	}
	if strings.Contains(buf.String(), "10.0.0.5") {
		t.Errorf("leaked address: %s", buf.String())
	}
}

// stackErr 模擬帶堆疊的 error：%+v 在 Error() 後多附 STACK-DETAIL.
type stackErr struct{}

func (stackErr) Error() string { return "dial tcp " + testAddr + ": connection refused" }

func (e stackErr) Format(f fmt.State, verb rune) {
	_, _ = io.WriteString(f, e.Error())
	if verb == 'v' && f.Flag('+') {
		_, _ = io.WriteString(f, " STACK-DETAIL")
	}
}

func initForTest(t *testing.T) string {
	t.Helper()
	restoreDefault(t)
	path := filepath.Join(t.TempDir(), "app.log")
	if err := Init("info", path); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(Close)
	return path
}

// F24-92: Init 真的用了出口.
func TestInitWritesRedactedLog(t *testing.T) {
	path := initForTest(t)
	slog.Warn("probe", "error", dialErr())
	Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range parseLines(t, data) {
		if m["msg"] != "probe" {
			continue
		}
		found = true
		wantMasked(t, "error", asString(t, m, "error"))
	}
	if !found {
		t.Fatalf("probe record not found in %s", data)
	}
}

// F24-93: go-redis 內建 log 轉進 slog 並經出口.
func TestRedisLoggerGoesThroughSlog(t *testing.T) {
	restoreDefault(t)
	var buf bytes.Buffer
	slog.SetDefault(slog.New(newHandler(&buf, slog.LevelInfo)))

	redisLogger{}.Printf(context.Background(), "redis: dial tcp %s: connect: connection refused", testAddr)

	lines := parseLines(t, buf.Bytes())
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if got := asString(t, lines[0], "msg"); got != "redis client" {
		t.Errorf("msg = %q", got)
	}
	wantMasked(t, "detail", asString(t, lines[0], "detail"))
}

// F24-93: Init 之後 go-redis 的真實失敗 log 進正式 log.
func TestInitRoutesRedisLogs(t *testing.T) {
	path := initForTest(t)

	// go-redis 全域 logger 不還原：redisLogger 轉到 slog.Default，後者已由 t.Cleanup 還原.
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	_ = c.Ping(context.Background()).Err() // 只為觸發 go-redis 內建 log，失敗是預期.
	if err := c.Close(); err != nil {
		t.Logf("close redis client: %v", err)
	}
	Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "127.0.0.1:1") {
		t.Errorf("log leaked loopback address: %s", data)
	}
	n := 0
	for _, m := range parseLines(t, data) {
		if m["msg"] == "redis client" {
			n++
		}
	}
	if n == 0 {
		t.Errorf("no 'redis client' record in %s", data)
	}
}

// captureStdout [F292-05] 執行 fn 並回傳它寫到標準輸出的內容.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prevOut, prevLog := os.Stdout, slog.Default()
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = prevOut; slog.SetDefault(prevLog); Close(); fileWriter = nil })
	fn()
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

// TestUnwritableLogFileFallsBackToStdout [F292-05] log 檔寫不了（例如 Render 的 Go 環境不能建立 /app）時不能讓服務啟動失敗，改成只輸出到標準輸出.
func TestUnwritableLogFileFallsBackToStdout(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "log", "app.log") // 上層是檔案，資料夾一定建不起來

	out := captureStdout(t, func() {
		if err := Init("info", path); err != nil {
			t.Errorf("Init must not fail: %v", err)
		}
		slog.Info("hello from the app")
	})

	if !strings.Contains(out, "log file unavailable") || !strings.Contains(out, "hello from the app") {
		t.Errorf("stdout = %s", out)
	}
	if fileWriter != nil {
		t.Error("no file writer when the file cannot be written")
	}
}

// TestWritableLogFileGetsTheLogsToo [F292-05] 寫得了檔時，檔案與標準輸出都有同一筆 log，且沒有警告.
func TestWritableLogFileGetsTheLogsToo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log", "app.log")
	out := captureStdout(t, func() {
		if err := Init("info", path); err != nil {
			t.Fatal(err)
		}
		slog.Info("written to both")
		Close()
	})
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "written to both") || !strings.Contains(out, "written to both") {
		t.Errorf("file=%q err=%v stdout=%q", data, err, out)
	}
	if strings.Contains(out, "log file unavailable") {
		t.Errorf("no warning when the file works: %s", out)
	}
}

// TestUnwritableLogFileFallbackStillRedacts [F292-05] 備援到標準輸出的路徑仍經出口遮蔽（F24-92）.
func TestUnwritableLogFileFallbackStillRedacts(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "log", "app.log")
	out := captureStdout(t, func() {
		if err := Init("info", path); err != nil {
			t.Errorf("Init must not fail: %v", err)
		}
		slog.Warn("probe", "error", dialErr())
	})
	found := false
	for _, m := range parseLines(t, []byte(out)) {
		if m["msg"] != "probe" {
			continue
		}
		found = true
		wantMasked(t, "error", asString(t, m, "error"))
	}
	if !found {
		t.Fatalf("probe record not found in %s", out)
	}
}

// TestEmptyLogPathWritesStdoutOnly [F292-05] 空路徑 = 不寫檔、不警告，且不沿用前一次 Init 留下的 fileWriter.
func TestEmptyLogPathWritesStdoutOnly(t *testing.T) {
	fileWriter = &lumberjack.Logger{Filename: filepath.Join(t.TempDir(), "stale.log")}
	out := captureStdout(t, func() {
		if err := Init("info", ""); err != nil {
			t.Errorf("Init must not fail: %v", err)
		}
		slog.Info("hello")
	})
	if !strings.Contains(out, "hello") || strings.Contains(out, "log file unavailable") {
		t.Errorf("stdout = %s", out)
	}
	if fileWriter != nil {
		t.Error("empty path must not keep a file writer")
	}
}

// TestUnopenableLogFileFallsBackToStdout [F292-05] 資料夾在但檔案開不起來（路徑本身是資料夾）也只寫標準輸出.
func TestUnopenableLogFileFallsBackToStdout(t *testing.T) {
	path := t.TempDir()
	out := captureStdout(t, func() {
		if err := Init("info", path); err != nil {
			t.Errorf("Init must not fail: %v", err)
		}
		slog.Info("hello from the app")
	})
	if !strings.Contains(out, "log file unavailable") || !strings.Contains(out, "hello from the app") {
		t.Errorf("stdout = %s", out)
	}
	if fileWriter != nil {
		t.Error("no file writer when the file cannot be opened")
	}
}

// TestParseLevel [F292-05] level 字串對照；不認得的字串用 info.
func TestParseLevel(t *testing.T) {
	tests := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"verbose", slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := parseLevel(tt.in); got != tt.want {
				t.Errorf("parseLevel(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestInitAppliesLevel [F292-05] Init 把 parseLevel(level) 接給 handler：warn 層級不輸出 info.
func TestInitAppliesLevel(t *testing.T) {
	out := captureStdout(t, func() {
		if err := Init("warn", ""); err != nil {
			t.Errorf("Init must not fail: %v", err)
		}
		slog.Info("quiet line")
		slog.Warn("loud line")
	})
	if !strings.Contains(out, "loud line") || strings.Contains(out, "quiet line") {
		t.Errorf("stdout = %s", out)
	}
}

// TestWritableLogFileKeepsRotationSettings [F292-05] 寫得了檔時沿用輪替設定（檔名、大小、天數、份數、壓縮）.
func TestWritableLogFileKeepsRotationSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	captureStdout(t, func() {
		if err := Init("info", path); err != nil {
			t.Fatal(err)
		}
	})
	w := fileWriter
	if w == nil || w.Filename != path || w.MaxSize != 100 || w.MaxAge != 7 || w.MaxBackups != 5 || !w.Compress {
		t.Errorf("rotation settings = %+v", w)
	}
}
