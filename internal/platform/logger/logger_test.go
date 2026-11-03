package logger

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout 執行 fn 並回傳它寫到標準輸出的內容.
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

// log 檔寫不了（例如 Render 的 Go 環境不能建立 /app）時不能讓服務啟動失敗，改成只輸出到標準輸出.
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
