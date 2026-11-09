package localcache

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// [F24-13] 對應單號的測試.
func TestCounterIncr(t *testing.T) {
	clk := newFakeClock()
	c := newCounter(testCfg("c", 0, clk), 100)
	for want := int64(1); want <= 3; want++ {
		if got := c.Incr("c", time.Hour); got != want {
			t.Fatalf("Incr = %d, want %d", got, want)
		}
	}
	if got := c.Incr("other", time.Hour); got != 1 {
		t.Fatalf("independent key must start at 1, got %d", got)
	}
	// ttl<=0：回 1 且不存入.
	for i := 0; i < 2; i++ {
		if got := c.Incr("z", 0); got != 1 {
			t.Fatalf("Incr(z, 0) call %d = %d, want 1", i, got)
		}
	}
	if got := c.Incr("neg", -time.Second); got != 1 {
		t.Fatalf("negative ttl = %d, want 1", got)
	}
	if got := c.Incr("z", time.Hour); got != 1 {
		t.Fatalf("Incr(z, 1h) after ttl<=0 calls = %d, want 1 (nothing stored)", got)
	}
}

// [F24-13] 對應單號的測試.
func TestCounterTTLNotExtended(t *testing.T) {
	clk := newFakeClock()
	c := newCounter(testCfg("c", 0, clk), 100)
	if got := c.Incr("c", 10*time.Second); got != 1 {
		t.Fatalf("first = %d", got)
	}
	clk.Advance(6 * time.Second)
	if got := c.Incr("c", 10*time.Second); got != 2 {
		t.Fatalf("second = %d, want 2", got)
	}
	clk.Advance(5 * time.Second) // 距第一次 11s.
	if got := c.Incr("c", 10*time.Second); got != 1 {
		t.Fatalf("third = %d, want 1 (deadline fixed by first Incr)", got)
	}
	// 過期後重建改用新 ttl.
	clk.Advance(8 * time.Second)
	if got := c.Incr("c", 10*time.Second); got != 2 {
		t.Fatalf("recreated entry must still be alive at +8s, got %d", got)
	}
}

// [F24-13] 對應單號的測試.
func TestCounterConcurrent(t *testing.T) {
	clk := newFakeClock()
	c := newCounter(testCfg("c", 0, clk), 1000)
	const g = 200
	res := make([]int, g)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < g; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res[i] = int(c.Incr("n", time.Hour))
		}()
	}
	close(start)
	wg.Wait()
	sort.Ints(res)
	for i, v := range res {
		if v != i+1 {
			t.Fatalf("results not exactly {1..%d}: res[%d]=%d", g, i, v)
		}
	}
	if got := c.Incr("n", time.Hour); got != g+1 {
		t.Fatalf("next Incr = %d, want %d", got, g+1)
	}
}

// [F24-14] 對應單號的測試.
func TestNoEvictKeepsAllUntilTTL(t *testing.T) {
	const n = 20000
	clk := newFakeClock()
	c := newNoEvict[int](testCfg("r", 0, clk), time.Hour, 1000000)
	b := newPure[int](testCfg("b", 100, clk), time.Hour)
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("k%d", i)
		c.SetIfAbsent(k, i, time.Hour)
		b.Set(k, i)
	}
	c.cleanUp()
	b.cleanUp()
	// 正對照：同負載下有容量的純快取確實會淘汰.
	if got := b.size(); got > 100 {
		t.Fatalf("control: New(100) size = %d, want <= 100", got)
	}
	for i := 0; i < n; i++ {
		if _, ok := c.Get(fmt.Sprintf("k%d", i)); !ok {
			t.Fatalf("key k%d evicted before TTL", i)
		}
	}
	clk.Advance(time.Hour + time.Second)
	for i := 0; i < n; i++ {
		if _, ok := c.Get(fmt.Sprintf("k%d", i)); ok {
			t.Fatalf("key k%d still present after TTL", i)
		}
	}
}

// [F24-14] 對應單號的測試.
func TestNoEvictWarnAboveExpected(t *testing.T) {
	rec := captureLogs(t)
	clk := newFakeClock()
	c := newNoEvict[int](testCfg("r", 0, clk), time.Hour, 10)
	for i := 0; i < 10; i++ {
		c.Set(fmt.Sprintf("k%d", i), i)
	}
	if got := rec.warns("above expected"); len(got) != 0 {
		t.Fatalf("10 entries (not above 10) must not Warn, got %v", got)
	}
	// 回 false 的 SetIfAbsent 不算寫入.
	c.SetIfAbsent("k0", 0, time.Hour)
	if got := rec.warns("above expected"); len(got) != 0 {
		t.Fatalf("failed SetIfAbsent must not Warn, got %v", got)
	}
	c.Set("k10", 10)
	got := rec.warns("above expected")
	if len(got) != 1 {
		t.Fatalf("11th write must Warn once, got %d: %v", len(got), got)
	}
	for _, want := range []string{"cache=r", "size=11", "expected=10"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("Warn %q missing %q", got[0], want)
		}
	}
	if strings.Contains(got[0], "k10") {
		t.Fatalf("Warn must not contain keys: %q", got[0])
	}
	for i := 0; i < 100; i++ {
		c.Set(fmt.Sprintf("m%d", i), i)
	}
	if got := rec.warns("above expected"); len(got) != 1 {
		t.Fatalf("rate limit broken: %d Warns in the same minute", len(got))
	}
	clk.Advance(61 * time.Second)
	c.Set("late", 1)
	if got := rec.warns("above expected"); len(got) != 2 {
		t.Fatalf("after 61s want 2 Warns, got %d", len(got))
	}
}

// [F24-15] 對應單號的測試.
func TestCounterNoEvict(t *testing.T) {
	const n = 20000
	clk := newFakeClock()
	c := newCounter(testCfg("c", 0, clk), 1000000)
	b := newPure[int](testCfg("b", 100, clk), time.Hour)
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("k%d", i)
		if got := c.Incr(k, time.Hour); got != 1 {
			t.Fatalf("first Incr(%s) = %d", k, got)
		}
		b.Set(k, i)
	}
	c.cleanUp()
	b.cleanUp()
	// 正對照：同負載下有容量的純快取確實會淘汰.
	if got := b.size(); got > 100 {
		t.Fatalf("control: New(100) size = %d, want <= 100", got)
	}
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("k%d", i)
		if got := c.Incr(k, time.Hour); got != 2 {
			t.Fatalf("second Incr(%s) = %d, want 2 (count evicted)", k, got)
		}
	}

	// Warn 與限流.
	rec := captureLogs(t)
	wclk := newFakeClock()
	w := NewCounterWithClock("c", 10, wclk.Now)
	for i := 0; i < 10; i++ {
		w.Incr(fmt.Sprintf("k%d", i), time.Hour)
	}
	if got := rec.warns("above expected"); len(got) != 0 {
		t.Fatalf("10 keys (not above 10) must not Warn, got %v", got)
	}
	w.Incr("k10", time.Hour)
	got := rec.warns("above expected")
	if len(got) != 1 || !strings.Contains(got[0], `cache=c`) {
		t.Fatalf("11th key must Warn once with name, got %v", got)
	}
	for i := 0; i < 100; i++ {
		w.Incr(fmt.Sprintf("m%d", i), time.Hour)
	}
	if got := rec.warns("above expected"); len(got) != 1 {
		t.Fatalf("rate limit broken: %d Warns in the same minute", len(got))
	}
}

// [F24-16] 對應單號的測試.
func TestInvalidArgsPanic(t *testing.T) {
	tests := []struct {
		name string
		fn   func()
	}{
		{"New maxSize 0", func() { New[int]("x", 0, time.Minute) }},
		{"New maxSize negative", func() { New[int]("x", -1, time.Minute) }},
		{"New defaultTTL 0", func() { New[int]("x", 10, 0) }},
		{"NewNoEvict defaultTTL 0", func() { NewNoEvict[int]("x", 0, 10) }},
		{"NewNoEvict warnAbove 0", func() { NewNoEvict[int]("x", time.Minute, 0) }},
		{"NewCounter warnAbove 0", func() { NewCounter("x", 0) }},
		{"NewWithClock maxSize 0", func() { NewWithClock[int]("x", 0, time.Minute, time.Now) }},
		{"NewNoEvictWithClock warnAbove 0", func() { NewNoEvictWithClock[int]("x", time.Minute, 0, time.Now) }},
		{"NewCounterWithClock warnAbove 0", func() { NewCounterWithClock("x", 0, time.Now) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("expected panic")
				}
				if msg := fmt.Sprint(r); !strings.Contains(msg, `"x"`) {
					t.Fatalf("panic message must contain the name, got %q", msg)
				}
			}()
			tt.fn()
		})
	}
	// 正對照：合法參數不得 panic.
	New[int]("x", 1, time.Minute)
	NewNoEvict[int]("x", time.Minute, 1)
	NewCounter("x", 1)
}

// [F241-11] 對應單號的測試：依條件刪除計數，被刪的重新從 1 起算，其餘不受影響.
func TestCounterDeleteMatching(t *testing.T) {
	endsU1 := func(k string) bool { return strings.HasSuffix(k, ":U1") }
	tests := []struct {
		name      string
		match     func(string) bool
		wantN     int
		wantNextA int64
		wantNextC int64
	}{
		{"deletes only matching", endsU1, 1, 1, 2},
		{"always false deletes nothing", func(string) bool { return false }, 0, 3, 2},
		{"always true deletes all", func(string) bool { return true }, 2, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCounter(testCfg("c", 0, newFakeClock()), 100)
			c.Incr("a:U1", time.Hour)
			c.Incr("a:U1", time.Hour)
			c.Incr("c:U2", time.Hour)
			if got := c.DeleteMatching(tt.match); got != tt.wantN {
				t.Fatalf("DeleteMatching = %d, want %d", got, tt.wantN)
			}
			if got := c.Incr("a:U1", time.Hour); got != tt.wantNextA {
				t.Errorf("Incr(a:U1) = %d, want %d", got, tt.wantNextA)
			}
			if got := c.Incr("c:U2", time.Hour); got != tt.wantNextC {
				t.Errorf("Incr(c:U2) = %d, want %d", got, tt.wantNextC)
			}
		})
	}
}
