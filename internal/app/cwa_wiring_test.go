package app

import (
	"bytes"
	"chatbot-go/internal/platform/config"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// F30-37：時區載入失敗時 build 不回錯、cwaLedger 非 nil、taipei 為 nil，且只記一行 Error.
func TestBuildKeepsRunningWhenTaipeiUnavailable(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	prev := loadTaipei
	loadTaipei = func() (*time.Location, error) { return nil, errors.New("lookup tzdata at 10.1.2.3 failed") }
	t.Cleanup(func() { loadTaipei = prev })

	a, err := build(&config.Config{}, unreachableDB(t), nil)
	if err != nil {
		t.Fatalf("build must not fail when Taipei is unavailable: %v", err)
	}
	if a.taipei != nil {
		t.Fatalf("taipei = %v, want nil", a.taipei)
	}
	if a.cwaLedger == nil {
		t.Fatal("cwaLedger must be created even without a timezone")
	}
	out := buf.String()
	if strings.Count(out, "taipei timezone unavailable, CWA schedulers disabled") != 1 || !strings.Contains(out, "level=ERROR") {
		t.Fatalf("want exactly one Error line, got: %s", out)
	}
	if strings.Contains(out, "10.1.2.3") {
		t.Fatalf("log leaked address: %s", out)
	}
}

// F30-37：時區載入成功時 app 帶著它.
func TestBuildLoadsTaipeiByDefault(t *testing.T) {
	a, err := build(&config.Config{}, unreachableDB(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.taipei == nil || a.taipei.String() != "Asia/Taipei" || a.cwaLedger == nil {
		t.Fatalf("taipei=%v ledger=%v", a.taipei, a.cwaLedger)
	}
}
