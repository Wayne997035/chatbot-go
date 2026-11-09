package integration

import (
	"chatbot-go/internal/alert"
	"chatbot-go/internal/platform/config"
	"context"
	"fmt"
	"slices"
	"testing"
)

// [F241-11] 真 Redis：glob 跳脫、SCAN 換頁、:tries 樣式都以真實 MATCH 語意驗證（單元測試的假 Redis 只是替身）.
func TestAlertHistoryForgetRecipientRealRedis(t *testing.T) {
	client := setupTestRedis(t)
	ctx := context.Background()
	const p = "alert:sent:earthquake:5:"

	own := []string{p + "U1", p + "U1:tries", p + "U*", p + "U*:tries"}
	keep := []string{p + "UA", p + "XU1", p + "UA:tries"}
	for _, k := range append(slices.Clone(own), keep...) {
		if err := client.Set(ctx, k, "1", 0).Err(); err != nil {
			t.Fatalf("seed %q: %v", k, err)
		}
	}
	// 300 把無關鍵：SCAN COUNT 100 必須換頁，U1 的鍵不會都落在第一頁.
	noise := make([]string, 0, 300)
	for i := range 300 {
		k := fmt.Sprintf("alert:sent:weather:台北市:大雨特報:N%03d", i)
		noise = append(noise, k)
		if err := client.Set(ctx, k, "1", 0).Err(); err != nil {
			t.Fatalf("seed noise: %v", err)
		}
	}

	checker := alert.NewChecker(nil, nil, client, &config.Config{})
	for _, uid := range []string{"U1", "U*"} {
		if err := checker.ForgetRecipient(ctx, uid); err != nil {
			t.Fatalf("ForgetRecipient(%q) = %v", uid, err)
		}
	}

	got, err := client.Keys(ctx, "alert:sent:*").Result()
	if err != nil {
		t.Fatalf("KEYS: %v", err)
	}
	want := append(slices.Clone(keep), noise...)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("remaining keys = %d, want %d (keep + noise)\nown keys left: %v", len(got), len(want), intersect(got, own))
	}
}

func intersect(a, b []string) []string {
	var out []string
	for _, k := range a {
		if slices.Contains(b, k) {
			out = append(out, k)
		}
	}
	return out
}
