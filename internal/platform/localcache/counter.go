package localcache

import (
	"fmt"
	"time"

	"github.com/maypok86/otter/v2"
)

// Counter 是 goroutine-safe 的原子計數器，不因容量淘汰（計數歸零 = 無限重試或上限失效）.
type Counter struct {
	name      string
	c         *otter.Cache[string, item[int64]]
	warnAbove int
	lim       *limiter
}

// [F24-15][F24-16] NewCounter 建計數器：筆數超過 warnAbove 只發 Warn（限流每分鐘 1 筆）；warnAbove<=0 會 panic.
func NewCounter(name string, warnAbove int) *Counter {
	return NewCounterWithClock(name, warnAbove, nil)
}

// [F24-15][F24-16] NewCounterWithClock 同 NewCounter，多一個注入時鐘（測試用）.
func NewCounterWithClock(name string, warnAbove int, now func() time.Time) *Counter {
	return newCounter(config{name: name, now: now}, warnAbove)
}

func newCounter(cfg config, warnAbove int) *Counter {
	if warnAbove <= 0 {
		panic(fmt.Sprintf("localcache %q: warnAbove must be > 0, got %d", cfg.name, warnAbove))
	}
	cfg.maxSize = 0 // [F24-15] 0 = 不設上限，只靠 TTL.
	lim := &limiter{now: nowFunc(cfg.now)}
	return &Counter{name: cfg.name, c: newStore[int64](cfg, lim), warnAbove: warnAbove, lim: lim}
}

// [F24-13] Incr 原子遞增並回傳遞增後的值. 不存在或已過期從 1 起算、TTL=ttl；已存在 +1，到期時間不變.
// ttl<=0 回 1 且不存入.
func (c *Counter) Incr(key string, ttl time.Duration) int64 {
	if ttl <= 0 {
		return 1
	}
	// Compute 回呼內 NEVER 呼叫任何 cache 方法（會死鎖）.
	got, _ := c.c.Compute(key, func(old item[int64], found bool) (item[int64], otter.ComputeOp) {
		if !found {
			return item[int64]{v: 1, ttl: ttl, keep: true}, otter.WriteOp
		}
		return item[int64]{v: old.v + 1, ttl: old.ttl, keep: true}, otter.WriteOp
	})
	warnIfAbove(c.name, c.c.EstimatedSize(), c.warnAbove, c.lim)
	return got.v
}

func (c *Counter) cleanUp() { c.c.CleanUp() }
