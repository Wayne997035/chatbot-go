package alert

import (
	"bytes"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// redactLogCapture 把預設 slog 換成不經全域 ReplaceAttr 的 TextHandler（只量第一層遮蔽），結束時還原.
func redactLogCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// redactLogAssert 斷言 log 有訊息與遮蔽記號、且不含任何原文.
func redactLogAssert(t *testing.T, got, msg string, raw, marks []string) {
	t.Helper()
	if !strings.Contains(got, msg) {
		t.Fatalf("log missing message %q: %s", msg, got)
	}
	for _, r := range raw {
		if strings.Contains(got, r) {
			t.Errorf("log leaks %q: %s", r, got)
		}
	}
	for _, m := range marks {
		if !strings.Contains(got, m) {
			t.Errorf("log missing redaction mark %q: %s", m, got)
		}
	}
}

// redactLogPushErrNotifier 推播一律回含 email 與位址的錯誤（既有 fakeNotifier 的錯誤寫死，不能用）.
type redactLogPushErrNotifier struct{}

func (redactLogPushErrNotifier) Push(context.Context, string, string) error {
	return errors.New("push failed for a@b.test at 10.0.0.5:6379")
}

// TestCountySubsLogRedactsRepoError [F303-09] 查縣市訂閱失敗時，log 的 error 欄經遮蔽，不含 email 與位址原文.
func TestCountySubsLogRedactsRepoError(t *testing.T) {
	buf := redactLogCapture(t)
	repo := &fakeSubs{err: errors.New("find failed for a@b.test at 10.0.0.5:27017")}
	c := NewChecker(repo, &fakeNotifier{}, nil, &config.Config{})
	if got := c.countySubs(context.Background(), "臺北市"); got != nil {
		t.Fatalf("countySubs = %v, want nil on repo error", got)
	}
	redactLogAssert(t, buf.String(), "find weather warning subs", []string{"a@b.test", "10.0.0.5"}, []string{"[email]", "[addr]"})
}

// TestClaimTsunamiTriggerLogRedactsStoreError [F303-09] Redis 連不上時，退記憶體的 warn log 經遮蔽，不含位址原文.
func TestClaimTsunamiTriggerLogRedactsStoreError(t *testing.T) {
	buf := redactLogCapture(t)
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() {
		if err := rdb.Close(); err != nil {
			t.Logf("close redis: %v", err)
		}
	})
	c := NewChecker(&fakeSubs{}, &fakeNotifier{}, rdb, &config.Config{})
	if !c.claimTsunamiTrigger(context.Background(), 1001) {
		t.Fatal("first claim must fall back to memory and succeed")
	}
	redactLogAssert(t, buf.String(), "tsunami trigger store unavailable", []string{"127.0.0.1"}, []string{"[addr]"})
}

// TestDeliverLogRedactsPushError [F303-09] LINE 推播失敗時，log 的 error 欄經遮蔽，不含 email 與位址原文.
func TestDeliverLogRedactsPushError(t *testing.T) {
	buf := redactLogCapture(t)
	c := NewChecker(&fakeSubs{}, redactLogPushErrNotifier{}, nil, &config.Config{})
	subs := []alertsub.AlertSubscription{{UserID: "U-redact-1"}}
	if n := c.deliver(context.Background(), "weather", "id-1", "msg", subs); n != 1 {
		t.Fatalf("deliver = %d, want 1 failed push counted", n)
	}
	redactLogAssert(t, buf.String(), "push alert", []string{"a@b.test", "10.0.0.5"}, []string{"[email]", "[addr]"})
}
