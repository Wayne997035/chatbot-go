package background

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 關機要等進行中的工作做完才返回（以前直接結束，處理到一半的訊息會不見）.
func TestShutdownWaitsForRunningWork(t *testing.T) {
	g := New(0, 0)
	release := make(chan struct{})
	var finished atomic.Bool
	g.Go(func(context.Context) {
		<-release
		finished.Store(true)
	})

	result := make(chan error, 1)
	go func() { result <- g.Shutdown(context.Background()) }()
	select {
	case err := <-result:
		t.Fatalf("shutdown returned before the work finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-result; err != nil || !finished.Load() {
		t.Errorf("err=%v finished=%v", err, finished.Load())
	}
}

// 排隊中的工作在關機時也要做完.
func TestQueuedWorkStillRunsOnShutdown(t *testing.T) {
	g := New(1, 10)
	release := make(chan struct{})
	var ran atomic.Int32
	for range 3 {
		g.Go(func(context.Context) {
			<-release
			ran.Add(1)
		})
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()
	if err := g.Shutdown(context.Background()); err != nil || ran.Load() != 3 {
		t.Errorf("err=%v ran=%d", err, ran.Load())
	}
}

// 執行中加排隊中超過上限時拒絕（不會無限制地堆 goroutine）.
func TestGoRejectsWhenTheQueueIsFull(t *testing.T) {
	g := New(1, 2)
	release := make(chan struct{})
	block := func(context.Context) { <-release }
	first, second := g.Go(block), g.Go(block)
	if !first || !second {
		t.Fatal("the first two fit")
	}
	if g.Go(block) {
		t.Error("the third must be rejected")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := g.Shutdown(ctx); err != nil {
		t.Errorf("accepted work must finish and release its queue slot: %v", err)
	}
}

// 只有 workers 個同時執行.
func TestWorkersLimitConcurrency(t *testing.T) {
	g := New(2, 0)
	var running, peak atomic.Int32
	var mu sync.Mutex
	for range 10 {
		g.Go(func(context.Context) {
			n := running.Add(1)
			mu.Lock()
			if n > peak.Load() {
				peak.Store(n)
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			running.Add(-1)
		})
	}
	_ = g.Shutdown(context.Background())
	if peak.Load() != 2 {
		t.Errorf("peak concurrency = %d, want 2", peak.Load())
	}
}

func TestNothingStartsAfterShutdown(t *testing.T) {
	g := New(0, 0)
	_ = g.Shutdown(context.Background())
	if g.Go(func(context.Context) { t.Error("must not run") }) {
		t.Error("Go after shutdown")
	}
	if g.Do(func(context.Context) { t.Error("must not run") }) {
		t.Error("Do after shutdown")
	}
}

// 逾時：取消剩下的工作（AI 呼叫等會收到 ctx 取消），回傳逾時錯誤.
func TestShutdownTimeoutCancelsTheWork(t *testing.T) {
	g := New(0, 0)
	canceled := make(chan struct{})
	g.Go(func(ctx context.Context) {
		<-ctx.Done()
		close(canceled)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := g.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
	select {
	case <-canceled:
	default:
		t.Error("the running work must see its context canceled")
	}
}

// Do（排程器的工作）也納入等待.
func TestShutdownWaitsForDo(t *testing.T) {
	g := New(0, 0)
	started := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	go g.Do(func(context.Context) {
		close(started)
		<-release
		finished.Store(true)
	})
	<-started
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()
	if err := g.Shutdown(context.Background()); err != nil || !finished.Load() {
		t.Errorf("err=%v finished=%v", err, finished.Load())
	}
}

// 同時送出工作與關機（-race 檢查 WaitGroup 的使用）：被接受的工作一定會跑完.
func TestConcurrentGoAndShutdown(t *testing.T) {
	for range 50 {
		g := New(4, 100)
		var accepted, ran atomic.Int32
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				if g.Go(func(context.Context) { ran.Add(1) }) {
					accepted.Add(1)
				}
			})
		}
		_ = g.Shutdown(context.Background())
		wg.Wait()
		if accepted.Load() != ran.Load() {
			t.Fatalf("accepted %d but ran %d", accepted.Load(), ran.Load())
		}
	}
}
