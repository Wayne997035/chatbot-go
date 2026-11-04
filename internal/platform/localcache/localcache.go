// Package localcache 是程式內記憶體快取的共用層（包 otter v2），給 Redis 不可用時的備援使用.
//
// 三種建構，依資料性質選：
//   - New：純快取，有容量上限，滿了會淘汰（對話狀態、天氣等丟了只是重算）.
//   - NewNoEvict：正確性紀錄，不因容量淘汰，只在 TTL 到期或 Delete 時消失（警報已送、防重放、刪除標記）.
//   - NewCounter：正確性計數，同樣不淘汰（重試次數、每日次數上限）.
//
// 對外不出現任何 otter 型別，消費端不得 import otter.
package localcache

import (
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/maypok86/otter/v2"
)

// warnInterval 同一個實例的 Warn 至多每這麼久一筆，避免洗版.
const warnInterval = time.Minute

// item 把 TTL 跟值放在一起，讓「寫值 + 設 TTL」是單一原子寫入.
type item[V any] struct {
	v   V
	ttl time.Duration
	// keep 為 true 時，更新不改到期時間（Counter 的遞增）；false 時更新重置 TTL.
	keep bool
}

// expiry 是 write-based 到期：建立與更新依 item，讀取不延長.
// 回呼內 NEVER 呼叫任何 cache 方法（otter 文件警告會死鎖）.
type expiry[V any] struct{}

func (expiry[V]) ExpireAfterCreate(e otter.Entry[string, item[V]]) time.Duration {
	return e.Value.ttl
}

func (expiry[V]) ExpireAfterUpdate(e otter.Entry[string, item[V]], _ item[V]) time.Duration {
	if e.Value.keep {
		return e.ExpiresAfter()
	}
	return e.Value.ttl
}

func (expiry[V]) ExpireAfterRead(e otter.Entry[string, item[V]]) time.Duration {
	return e.ExpiresAfter()
}

// fnClock 把 func() time.Time 轉成 otter 的 Clock；Tick 回 nil 表示不做背景清理.
type fnClock struct{ now func() time.Time }

func (c fnClock) NowNano() int64                    { return c.now().UnixNano() }
func (fnClock) Tick(time.Duration) <-chan time.Time { return nil }

// limiter 限流：同一實例每 warnInterval 至多放行一次，計時走注入的時鐘.
type limiter struct {
	now  func() time.Time
	last atomic.Int64
}

func (l *limiter) allow() bool {
	n := l.now().UnixNano()
	for {
		p := l.last.Load()
		if p != 0 && n-p < int64(warnInterval) {
			return false
		}
		if l.last.CompareAndSwap(p, n) {
			return true
		}
	}
}

// config 是未匯出的建構參數；測試藉由 exec 取得同步的 OnDeletion.
type config struct {
	name    string
	maxSize int // 0 = 不設上限（只有 NewNoEvict 與 NewCounter 會傳 0）.
	// now 為 nil 時用真實時間與 otter 預設時鐘（會做背景清理）.
	now func() time.Time
	// exec 為 nil 時用 otter 預設 Executor；測試傳同步版.
	exec func(func())
}

// newStore 建出底層 otter 實例. 只有 maxSize>0 才掛淘汰告警.
func newStore[V any](cfg config, lim *limiter) *otter.Cache[string, item[V]] {
	opts := &otter.Options[string, item[V]]{
		MaximumSize:      cfg.maxSize,
		ExpiryCalculator: expiry[V]{},
		Executor:         cfg.exec,
	}
	if cfg.now != nil {
		opts.Clock = fnClock{now: cfg.now}
	}
	if cfg.maxSize > 0 {
		name := cfg.name
		opts.OnDeletion = func(e otter.DeletionEvent[string, item[V]]) {
			// [F24-11] 只印 cache name，NEVER 印 key 或 value.
			if e.Cause == otter.CauseOverflow && lim.allow() {
				slog.Warn("localcache evicted before expiry", "cache", name)
			}
		}
	}
	return otter.Must(opts)
}

func nowFunc(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}

// Cache 是 goroutine-safe 的字串 key 快取，到期為 write-based（讀不延長，Set 重置）.
type Cache[V any] struct {
	name       string
	c          *otter.Cache[string, item[V]]
	defaultTTL time.Duration
	warnAbove  int // 0 = 不檢查（純快取）.
	lim        *limiter
}

func newCache[V any](cfg config, defaultTTL time.Duration, warnAbove int) *Cache[V] {
	lim := &limiter{now: nowFunc(cfg.now)}
	return &Cache[V]{
		name:       cfg.name,
		c:          newStore[V](cfg, lim),
		defaultTTL: defaultTTL,
		warnAbove:  warnAbove,
		lim:        lim,
	}
}

// [F24-11][F24-16] New 建純快取：最多 maxSize 筆，滿了會淘汰；maxSize<=0 或 defaultTTL<=0 會 panic.
func New[V any](name string, maxSize int, defaultTTL time.Duration) *Cache[V] {
	return NewWithClock[V](name, maxSize, defaultTTL, nil)
}

// [F24-11][F24-16] NewWithClock 同 New，多一個注入時鐘（測試用）.
func NewWithClock[V any](name string, maxSize int, defaultTTL time.Duration, now func() time.Time) *Cache[V] {
	return newPure[V](config{name: name, maxSize: maxSize, now: now}, defaultTTL)
}

func newPure[V any](cfg config, defaultTTL time.Duration) *Cache[V] {
	if cfg.maxSize <= 0 {
		panic(fmt.Sprintf("localcache %q: maxSize must be > 0 (use NewNoEvict for unbounded), got %d", cfg.name, cfg.maxSize))
	}
	if defaultTTL <= 0 {
		panic(fmt.Sprintf("localcache %q: defaultTTL must be > 0, got %s", cfg.name, defaultTTL))
	}
	return newCache[V](cfg, defaultTTL, 0)
}

// [F24-14][F24-16] NewNoEvict 建正確性紀錄：不因容量淘汰，筆數超過 warnAbove 只發 Warn（限流每分鐘 1 筆），不拒收.
// defaultTTL<=0 或 warnAbove<=0 會 panic.
func NewNoEvict[V any](name string, defaultTTL time.Duration, warnAbove int) *Cache[V] {
	return NewNoEvictWithClock[V](name, defaultTTL, warnAbove, nil)
}

// [F24-14][F24-16] NewNoEvictWithClock 同 NewNoEvict，多一個注入時鐘（測試用）.
func NewNoEvictWithClock[V any](name string, defaultTTL time.Duration, warnAbove int, now func() time.Time) *Cache[V] {
	return newNoEvict[V](config{name: name, now: now}, defaultTTL, warnAbove)
}

func newNoEvict[V any](cfg config, defaultTTL time.Duration, warnAbove int) *Cache[V] {
	if defaultTTL <= 0 {
		panic(fmt.Sprintf("localcache %q: defaultTTL must be > 0, got %s", cfg.name, defaultTTL))
	}
	if warnAbove <= 0 {
		panic(fmt.Sprintf("localcache %q: warnAbove must be > 0, got %d", cfg.name, warnAbove))
	}
	cfg.maxSize = 0 // [F24-14] 0 = 不設上限，只靠 TTL.
	return newCache[V](cfg, defaultTTL, warnAbove)
}

// [F24-11] Get 未命中或已過期回零值與 false；讀取不延長 TTL.
func (c *Cache[V]) Get(key string) (V, bool) {
	it, ok := c.c.GetIfPresent(key)
	if !ok {
		var zero V
		return zero, false
	}
	return it.v, true
}

// [F24-11] Set 等同 SetTTL(key, v, defaultTTL)；覆蓋既有 key 並重置 TTL.
func (c *Cache[V]) Set(key string, v V) {
	c.SetTTL(key, v, c.defaultTTL)
}

// [F24-11] SetTTL 以指定 TTL 寫入；ttl<=0 不存入（視為已過期），也不影響既有 entry.
func (c *Cache[V]) SetTTL(key string, v V, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.c.Set(key, item[V]{v: v, ttl: ttl})
	c.checkSize()
}

// [F24-12] SetIfAbsent 原子地「不存在（含已過期）才寫」：寫入回 true；已存在回 false，不改值也不改 TTL.
// ttl<=0 不存入並回 true（fail-open：防重放把 false 解讀為重放會丟訊息）.
func (c *Cache[V]) SetIfAbsent(key string, v V, ttl time.Duration) bool {
	if ttl <= 0 {
		return true
	}
	_, ok := c.c.SetIfAbsent(key, item[V]{v: v, ttl: ttl})
	if ok {
		c.checkSize()
	}
	return ok
}

// [F24-11] Delete 刪除 key；不存在時 no-op.
func (c *Cache[V]) Delete(key string) {
	c.c.Invalidate(key)
}

// [F24-14] checkSize 超過 warnAbove 時發 Warn（限流）. 只印 name、size、expected，NEVER 印 key 或 value.
func (c *Cache[V]) checkSize() {
	warnIfAbove(c.name, c.c.EstimatedSize(), c.warnAbove, c.lim)
}

func warnIfAbove(name string, size, warnAbove int, lim *limiter) {
	if warnAbove > 0 && size > warnAbove && lim.allow() {
		slog.Warn("localcache above expected size", "cache", name, "size", size, "expected", warnAbove)
	}
}

// cleanUp 同步跑完 otter 維護（測試同步點）.
func (c *Cache[V]) cleanUp() { c.c.CleanUp() }

// size 回存量估計（測試用）.
func (c *Cache[V]) size() int { return c.c.EstimatedSize() }
