package logger

import (
	"chatbot-go/internal/redact"
	"context"
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

// redactAttr [F24-92] 對所有屬性（含 group 內、msg）套用 redact；source、時間、等級、數字、布林不動.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	v := a.Value
	if addressKeys[strings.ToLower(a.Key)] && !isEmptyValue(v) {
		return slog.String(a.Key, "[addr]")
	}
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, redact.String(v.String()))
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, redact.Error(err))
		}
		if a.Key == slog.SourceKey {
			return a
		}
		s := fmt.Sprintf("%+v", v.Any())
		if r := redact.String(s); r != s {
			return slog.String(a.Key, r)
		}
	default:
	}
	return a
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
