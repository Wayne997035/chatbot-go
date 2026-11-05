package logger

import (
	"bytes"
	"chatbot-go/internal/redact"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/redis/go-redis/v9"
	"gopkg.in/natefinch/lumberjack.v2"
)

var fileWriter *lumberjack.Logger

func Init(level, filePath string) error {
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}

	fileWriter = &lumberjack.Logger{
		Filename:   filePath,
		MaxSize:    100,
		MaxAge:     7,
		MaxBackups: 5,
		Compress:   true,
	}

	multiWriter := io.MultiWriter(os.Stdout, fileWriter)

	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	slog.SetDefault(slog.New(newHandler(multiWriter, logLevel)))
	// [F24-93] go-redis 預設把 log 寫到 stderr 且不經 slog，改轉接讓它也經過 [F24-92] 的出口遮蔽.
	redis.SetLogger(redisLogger{})
	return nil
}

// newHandler 建立 JSON handler，ReplaceAttr 讓每個屬性在寫出前經過 redact.
func newHandler(w io.Writer, level slog.Level) slog.Handler {
	return slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		AddSource:   true,
		ReplaceAttr: redactAttr, // [F24-92]
	})
}

// addressKeys 是位址類屬性名（小寫），非空值整個換成 [addr].
var addressKeys = map[string]bool{
	"addr": true, "address": true, "host": true, "hostname": true,
	"url": true, "uri": true, "endpoint": true, "dsn": true,
}

// redactAttr [F24-92] 對所有屬性（含 group 內、msg、屬性名）套用 redact；source、時間、數字、布林不動.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if k := redact.String(a.Key); k != a.Key {
		a.Key = k
	}
	v := a.Value
	if addressKeys[strings.ToLower(a.Key)] && !isEmptyValue(v) {
		return slog.String(a.Key, "[addr]")
	}
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, redact.String(v.String()))
	case slog.KindAny:
		return redactAny(a)
	default:
	}
	return a
}

// redactAny [SEC-R3-02] 寫出的必須是檢查過的那一份. JSONHandler 用 json 編碼寫出非 error 的值，
// 所以 %+v（看得到欄位名）與 JSON（看得到 Stringer 藏起來的欄位）兩份都檢查；任一份有命中就改寫成
// 那份遮蔽後的字串，兩份都沒命中才原樣交給 handler.
func redactAny(a slog.Attr) slog.Attr {
	switch x := a.Value.Any().(type) {
	case *slog.Source:
		return a
	case error:
		return slog.String(a.Key, redact.Error(x))
	case json.RawMessage:
		return slog.String(a.Key, redact.String(string(x)))
	case []byte:
		return slog.String(a.Key, redact.String(string(x)))
	}
	v := a.Value.Any()
	pv := fmt.Sprintf("%+v", v)
	if rp := redact.String(pv); rp != pv {
		return slog.String(a.Key, rp)
	}
	js, err := jsonText(v)
	if err != nil {
		return slog.String(a.Key, redact.String(pv))
	}
	if rj := redact.String(js); rj != js {
		return slog.String(a.Key, rj)
	}
	return a
}

// jsonText 用和 JSONHandler 相同的設定編碼（不跳脫 HTML），去掉結尾換行.
func jsonText(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// ReportFatal [SEC-R3-02] 開機致命錯誤以 ERROR 寫出；Init 之前 slog 還是預設 handler（不經出口遮蔽），所以先遮蔽一次.
func ReportFatal(err error) {
	slog.Error("startup failed", "error", redact.Error(err))
}

// isEmptyValue 字串種類為空字串、KindAny 為 nil 才算空；其他種類一律視為非空.
func isEmptyValue(v slog.Value) bool {
	switch v.Kind() {
	case slog.KindString:
		return v.String() == ""
	case slog.KindAny:
		return v.Any() == nil
	default:
		return false
	}
}

// redisLogger [F24-93] 把 go-redis 內建 log 轉進 slog.Default，經出口遮蔽後才寫出.
type redisLogger struct{}

func (redisLogger) Printf(ctx context.Context, format string, v ...interface{}) {
	slog.WarnContext(ctx, "redis client", "detail", fmt.Sprintf(format, v...))
}

func Close() {
	if fileWriter != nil {
		_ = fileWriter.Close()
	}
}
