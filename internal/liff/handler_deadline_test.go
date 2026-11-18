package liff

import (
	"chatbot-go/internal/storage/database/profile"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// blockingProfiles [F34-30] Upsert 阻塞到 ctx 結束，或 3 秒到（保險，避免沒有期限時整個套件卡死）；記下收到的 ctx 狀態.
type blockingProfiles struct {
	mu          sync.Mutex
	hasDeadline bool
	remaining   time.Duration
}

func (b *blockingProfiles) Get(context.Context, string) (*profile.Profile, error) { return nil, nil }

func (b *blockingProfiles) Upsert(ctx context.Context, _ *profile.Profile) error {
	b.mu.Lock()
	if dl, ok := ctx.Deadline(); ok {
		b.hasDeadline, b.remaining = true, time.Until(dl)
	}
	b.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(3 * time.Second):
		return nil
	}
}

func (b *blockingProfiles) Delete(context.Context, string) error { return nil }

// TestPutProfileUpsertHasDeadline [F34-30] PutProfile 的 Upsert 收到有期限的 ctx（<=1 秒），慢寫入在 2 秒內就回應.
func TestPutProfileUpsertHasDeadline(t *testing.T) {
	bp := &blockingProfiles{}
	api := newProfileAPI(t, bp, nil)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- putProfile(t, context.Background(), api) }()

	select {
	case rec := <-done:
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500 when the write deadline expires: %s", rec.Code, rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PutProfile did not respond within 2s: Upsert ctx has no deadline")
	}

	bp.mu.Lock()
	defer bp.mu.Unlock()
	if !bp.hasDeadline {
		t.Fatal("Upsert ctx has no deadline")
	}
	if bp.remaining <= 0 || bp.remaining > time.Second {
		t.Errorf("Upsert ctx deadline remaining = %v, want in (0, 1s]", bp.remaining)
	}
}
