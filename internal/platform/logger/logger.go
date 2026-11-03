// Package logger 設定 slog：一律輸出到標準輸出（部署平台的 log 畫面看得到），
// 可以的話同時寫到輪替的 log 檔.
package logger

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"
)

var fileWriter *lumberjack.Logger

// Init 設定全域 logger. log 檔寫不了（例如部署平台沒有 /app 的寫入權限）時只輸出到標準輸出並警告，
// 不讓服務因此啟動失敗；filePath 為空表示不寫檔.
func Init(level, filePath string) error {
	writers := []io.Writer{os.Stdout}
	fileErr := openLogFile(filePath)
	if fileErr == nil && filePath != "" {
		fileWriter = &lumberjack.Logger{
			Filename:   filePath,
			MaxSize:    100,
			MaxAge:     7,
			MaxBackups: 5,
			Compress:   true,
		}
		writers = append(writers, fileWriter)
	}

	handler := slog.NewJSONHandler(io.MultiWriter(writers...), &slog.HandlerOptions{
		Level:     parseLevel(level),
		AddSource: true,
	})
	slog.SetDefault(slog.New(handler))

	if fileErr != nil {
		slog.Warn("log file unavailable, logging to stdout only", "path", filePath, "error", fileErr)
	}
	return nil
}

// openLogFile 確認 log 檔可以寫（資料夾建得起來、檔案開得起來）.
func openLogFile(filePath string) error {
	if filePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Clean(filePath), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Close 關閉 log 檔.
func Close() {
	if fileWriter != nil {
		_ = fileWriter.Close()
	}
}
