// Package background 追蹤背景工作：限制同時數量與排隊數量，關機時等它們做完，逾時才取消.
package background

import (
	"context"
	"sync"
	"time"
)

// cancelGrace 逾時取消後，再等多久讓工作收尾（工作應該在 ctx 取消後很快返回）.
const cancelGrace = 3 * time.Second

// Group 一組背景工作. 零值不能用，請用 New.
type Group struct {
	ctx    context.Context
	cancel context.CancelFunc
	slots  chan struct{} // 同時執行的上限；nil 表示不限
	queue  chan struct{} // 執行中 + 排隊中的上限；nil 表示不限

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// New 建立工作組. workers 是同時執行的上限，queued 是執行中加排隊中的上限；0 表示不限.
func New(workers, queued int) *Group {
	ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // cancel 存在 Group 裡，由 Shutdown 呼叫
	g := &Group{ctx: ctx, cancel: cancel}
	if workers > 0 {
		g.slots = make(chan struct{}, workers)
	}
	if queued > 0 {
		g.queue = make(chan struct{}, queued)
	}
	return g
}

// Go 在背景執行 fn；已經在關機或排隊已滿時不執行並回傳 false.
// fn 收到的 ctx 在關機逾時時取消，長時間的呼叫（AI、外部 API）要用它.
func (g *Group) Go(fn func(ctx context.Context)) bool {
	if !g.enter() {
		return false
	}
	go func() {
		defer g.leave()
		if g.slots != nil {
			select {
			case g.slots <- struct{}{}:
				defer func() { <-g.slots }()
			case <-g.ctx.Done():
				return
			}
		}
		fn(g.ctx)
	}()
	return true
}

// Do 在目前的 goroutine 執行 fn，並納入關機等待（排程器的工作用）；已經在關機時不執行並回傳 false.
// 不受 workers / queued 限制.
func (g *Group) Do(fn func(ctx context.Context)) bool {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return false
	}
	g.wg.Add(1)
	g.mu.Unlock()
	defer g.wg.Done()
	fn(g.ctx)
	return true
}

func (g *Group) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	if g.queue != nil {
		select {
		case g.queue <- struct{}{}:
		default:
			return false
		}
	}
	g.wg.Add(1)
	return true
}

func (g *Group) leave() {
	if g.queue != nil {
		<-g.queue
	}
	g.wg.Done()
}

// Shutdown 不再接受新工作，等進行中與排隊中的做完. ctx 到期時取消剩下的工作，
// 再最多等 cancelGrace 讓它們收尾，回傳 ctx 的錯誤.
func (g *Group) Shutdown(ctx context.Context) error {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()

	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		g.cancel()
		return nil
	case <-ctx.Done():
		g.cancel()
		select {
		case <-done:
		case <-time.After(cancelGrace):
		}
		return ctx.Err()
	}
}
