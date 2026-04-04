package logger

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"

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

	handler := slog.NewJSONHandler(multiWriter, &slog.HandlerOptions{
		Level:     logLevel,
		AddSource: true,
	})

	slog.SetDefault(slog.New(handler))
	return nil
}

func Close() {
	if fileWriter != nil {
		_ = fileWriter.Close()
	}
}
