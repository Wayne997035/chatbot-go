package dblog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"chatbot-go/internal/reqid"
)

func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func records(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// [F37-08] 成功：一行 INFO，collection / action / ok / req.
func TestWriteSuccessIsOneInfoLine(t *testing.T) {
	buf := capture(t)
	ctx := reqid.With(context.Background())
	want, _ := reqid.From(ctx)

	Write(ctx, "meal", ActionInsert, nil)

	recs := records(t, buf)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %s", len(recs), buf.String())
	}
	r := recs[0]
	if r["level"] != "INFO" || r["msg"] != "db write" || r["collection"] != "meal" || r["action"] != "insert" || r["ok"] != true || r["req"] != want {
		t.Errorf("record = %v", r)
	}
	if _, has := r["error_class"]; has {
		t.Error("success line must not carry error_class")
	}
}

// [F37-08] 失敗：ERROR + error_class（封閉集合），輸出不含錯誤訊息、不含 id.
func TestWriteFailureIsErrorWithClassOnly(t *testing.T) {
	const uid = "U0123456789abcdef0123456789abcdef"
	dup := mongo.WriteException{WriteErrors: mongo.WriteErrors{{Code: 11000, Message: `dup key: { _id: "` + uid + `" }`}}}
	tests := []struct {
		name  string
		err   error
		class string
	}{
		{"duplicate key", dup, ClassDuplicateKey},
		{"deadline", context.DeadlineExceeded, ClassTimeout},
		{"canceled", context.Canceled, ClassCanceled},
		{"other carrying a filter value", errors.New(`dup key: { _id: "` + uid + `" } ` + `E11000`), ClassOther},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buf := capture(t)
			Write(reqid.With(context.Background()), "user", ActionUpsert, tc.err)

			recs := records(t, buf)
			if len(recs) != 1 {
				t.Fatalf("got %d records: %s", len(recs), buf.String())
			}
			r := recs[0]
			if r["level"] != "ERROR" || r["ok"] != false || r["error_class"] != tc.class {
				t.Errorf("record = %v, want ERROR ok=false error_class=%s", r, tc.class)
			}
			out := buf.String()
			if strings.Contains(out, uid) || strings.Contains(out, "dup key") || strings.Contains(out, "E11000") {
				t.Errorf("log leaks the error text: %s", out)
			}
		})
	}
}

// [F37-08] 沒有請求代號的 ctx（如 cron）：沒有 req 鍵；nil ctx 不 panic.
func TestWriteWithoutRequestID(t *testing.T) {
	buf := capture(t)
	Write(context.Background(), "meal", ActionDelete, nil)
	//nolint:staticcheck // 刻意傳 nil ctx 驗證 helper 不 panic
	Write(nil, "meal", ActionDeleteMany, nil)

	out := buf.String()
	if strings.Contains(out, `"req"`) {
		t.Errorf("no id in ctx, but a req key was emitted: %s", out)
	}
	if n := len(regexp.MustCompile(`"msg":"db write"`).FindAllString(out, -1)); n != 2 {
		t.Errorf("got %d db write lines, want 2", n)
	}
}
