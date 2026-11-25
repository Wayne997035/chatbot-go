package server

import (
	"chatbot-go/internal/liff"
	"chatbot-go/internal/webhook"
	"testing"
	"time"
)

// [F37-06] S2：webhook 與 liff 各留一份 erasureOpTimeout（兩個套件互不 import），改其中一份而忘了另一份要紅.
func TestErasureOpTimeoutIsTheSameInWebhookAndLiff(t *testing.T) {
	if got, want := webhook.ErasureOpTimeout(), liff.ErasureOpTimeout(); got != want {
		t.Fatalf("webhook erasureOpTimeout = %v, liff erasureOpTimeout = %v; the two copies must stay equal", got, want)
	}
	if got := webhook.ErasureOpTimeout(); got != 5*time.Second {
		t.Fatalf("erasureOpTimeout = %v, want 5s", got)
	}
}
