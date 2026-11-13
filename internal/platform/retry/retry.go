// Package retry 共用的重試輔助：最多 N 次、等待遵守 ctx、由呼叫端決定哪些錯誤可重試.
package retry

import (
	"context"
	"time"
)

// Policy 重試策略. MaxAttempts 小於 1 視為 1；Retryable 為 nil 表示不重試.
type Policy struct {
	MaxAttempts int
	// Backoff 第 attempt 次失敗之後、下一次嘗試之前要等多久；nil 表示不等.
	Backoff func(attempt int) time.Duration
	// Sleep 等待函式；nil 用 SleepCtx. 測試可注入以免真的睡.
	Sleep func(ctx context.Context, d time.Duration) error
	// Retryable 判斷失敗的嘗試是否值得再試.
	Retryable func(error) bool
	// BeforeAttempt 每次嘗試之前呼叫（含第一次）；回錯則該次不發送、Do 以該錯誤結束.
	BeforeAttempt func(ctx context.Context, attempt int) error
}

// Do 執行 fn 最多 p.MaxAttempts 次（F30-30）. attempt 從 1 起算；
// 回傳的 attempts 是 fn 實際被呼叫的次數（被 BeforeAttempt 擋下或 ctx 結束的那次不算）.
func Do(ctx context.Context, p Policy, fn func(ctx context.Context, attempt int) error) (attempts int, err error) {
	maxAttempts := max(p.MaxAttempts, 1)
	sleep := p.Sleep
	if sleep == nil {
		sleep = SleepCtx
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			if p.Backoff != nil {
				if d := p.Backoff(attempt - 1); d > 0 {
					if serr := sleep(ctx, d); serr != nil {
						return attempts, serr
					}
				}
			}
		}
		if cerr := ctx.Err(); cerr != nil {
			return attempts, cerr
		}
		if p.BeforeAttempt != nil {
			if berr := p.BeforeAttempt(ctx, attempt); berr != nil {
				return attempts, berr
			}
		}
		attempts++
		err = fn(ctx, attempt)
		if err == nil {
			return attempts, nil
		}
		if attempt == maxAttempts || p.Retryable == nil || !p.Retryable(err) {
			return attempts, err
		}
	}
	return attempts, err
}

// SleepCtx 預設的等待：ctx 到期或取消立刻回 ctx.Err()（F30-30）.
func SleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
