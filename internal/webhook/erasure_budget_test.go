package webhook

import (
	"context"
	"testing"
	"time"
)

// TestRecheckAfterWriteUndoRespectsErasureBudget [F34-31] 請求取消後，補刪的 ctx 不立刻被切，但在整批預算內收掉；
// 守的是 recheckAfterWrite 的補刪 ctx 取自預算（erasure.go 的 erasureOpCtx），不是自己脫離後只剩每段期限.
func TestRecheckAfterWriteUndoRespectsErasureBudget(t *testing.T) {
	shortenErasureBudget(t, 60*time.Millisecond) // 與既有測試的 50ms 錯開：四處同值會被 unparam 判成常數參數
	h := NewWebhookHandler(nil, nil, nil, nil, nil)
	markErased(t, h.erased, context.Background(), guardUID)
	parent, cancel := context.WithCancel(sentMinuteAgo())
	defer cancel()
	ctx, done := withErasureBudget(parent)
	defer done()

	type result struct {
		erased bool
		err    error
	}
	got := make(chan context.Context, 1)
	released := make(chan time.Time, 1)
	resCh := make(chan result, 1)
	calls := 0
	go func() {
		erased, err := h.recheckAfterWrite(ctx, guardUID, func(cctx context.Context) error {
			calls++
			got <- cctx
			<-cctx.Done()
			released <- time.Now()
			return cctx.Err()
		})
		resCh <- result{erased, err}
	}()

	var cctx context.Context
	select {
	case cctx = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("undo was never called: the test would be vacuous")
	}
	if err := cctx.Err(); err != nil {
		t.Fatalf("undo ctx already done before cancel: %v", err)
	}
	cancel()
	if err := cctx.Err(); err != nil {
		t.Fatalf("undo ctx canceled together with the request: %v", err)
	}
	cancelAt := time.Now()

	select {
	case r := <-resCh:
		if !r.erased || calls != 1 {
			t.Fatalf("erased=%v undoCalls=%d, want true and 1", r.erased, calls)
		}
	case <-time.After(time.Second):
		t.Fatal("undo still blocked 1s after cancel: undo ctx is not bound by the erasure budget")
	}
	if d := (<-released).Sub(cancelAt); d >= time.Second {
		t.Errorf("undo returned %v after cancel, want < 1s", d)
	}
}

// [F34-32] TestRecheckAfterWriteUndoContextHasFiveSecondDeadline（erasure_guard_test.go）改為上下限都驗 (4s, 5s]：
// 該檔同時被其他組修改，標記寫在這裡；erasureOpTimeout 被改成過短或過長都會讓它紅.
