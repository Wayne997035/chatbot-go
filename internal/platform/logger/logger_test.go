package logger

import (
	"bytes"
	"context"
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
