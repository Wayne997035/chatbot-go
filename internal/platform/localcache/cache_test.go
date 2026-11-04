package localcache

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// [F24-11] 對應單號的測試.
func TestGetSet(t *testing.T) {
	clk := newFakeClock()
	c := newPure[int](testCfg("t", 10, clk), time.Minute)
	c.Set("a", 1)
	tests := []struct {
		name   string
		key    string
		want   int
		wantOK bool
	}{
		{"hit", "a", 1, true},
		{"never set returns zero value", "never", 0, false},
		{"other key unaffected", "b", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := c.Get(tt.key)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("Get(%q) = (%d,%v), want (%d,%v)", tt.key, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// [F24-11] 對應單號的測試.
func TestDefaultTTL(t *testing.T) {
	clk := newFakeClock()
	c := newPure[string](testCfg("t", 10, clk), time.Minute)
	c.Set("k", "v")
	if _, ok := c.Get("k"); !ok {
		t.Fatal("control: must hit before time advances")
	}
	clk.Advance(59 * time.Second)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("must hit at 59s")
	}
	clk.Advance(2 * time.Second)
	if v, ok := c.Get("k"); ok || v != "" {
		t.Fatalf("must miss at 61s, got (%q,%v)", v, ok)
	}
}

// [F24-11] 對應單號的測試.
func TestSetTTLPerEntry(t *testing.T) {
	clk := newFakeClock()
	c := newPure[string](testCfg("t", 10, clk), time.Hour)
	c.SetTTL("x", "v", 10*time.Second)
	c.SetTTL("y", "v", 100*time.Second)
	c.SetTTL("z", "v", 0)
	c.SetTTL("neg", "v", -time.Second)
	if _, ok := c.Get("z"); ok {
		t.Fatal("ttl<=0 must not be stored")
	}
	if _, ok := c.Get("neg"); ok {
		t.Fatal("negative ttl must not be stored")
	}
	c.Set("keep", "old")
	c.SetTTL("keep", "new", 0) // 不影響既有 entry.
	if v, ok := c.Get("keep"); !ok || v != "old" {
		t.Fatalf("ttl<=0 must not touch existing entry, got (%q,%v)", v, ok)
	}
	clk.Advance(50 * time.Second)
	if _, ok := c.Get("x"); ok {
		t.Fatal("x must be gone at 50s")
	}
	if _, ok := c.Get("y"); !ok {
		t.Fatal("y must still hit at 50s")
	}
}

// [F24-11] 對應單號的測試.
func TestSetResetsTTL(t *testing.T) {
	clk := newFakeClock()
	c := newPure[int](testCfg("t", 10, clk), 60*time.Second)
	c.Set("k", 1)
	clk.Advance(40 * time.Second)
	c.Set("k", 2)
	clk.Advance(30 * time.Second) // 距第一次 70s、距第二次 30s.
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Fatalf("second Set must reset TTL, got (%d,%v)", v, ok)
	}
	clk.Advance(31 * time.Second) // 距第二次 61s.
	if _, ok := c.Get("k"); ok {
		t.Fatal("must expire 60s after the second Set")
	}
}

// [F24-11] 對應單號的測試.
func TestGetDoesNotExtend(t *testing.T) {
	clk := newFakeClock()
	c := newPure[int](testCfg("t", 10, clk), 60*time.Second)
	c.Set("k", 1)
	for i := 0; i < 3; i++ {
		clk.Advance(20 * time.Second)
		if i < 2 {
			if _, ok := c.Get("k"); !ok {
				t.Fatalf("must hit at %ds", (i+1)*20)
			}
		}
	}
	clk.Advance(time.Second) // 61s.
	if _, ok := c.Get("k"); ok {
		t.Fatal("reads must not extend TTL")
	}
}

// [F24-12] 對應單號的測試.
func TestSetIfAbsent(t *testing.T) {
	clk := newFakeClock()
	c := newNoEvict[string](testCfg("t", 0, clk), time.Hour, 100)
	if !c.SetIfAbsent("k", "v1", 5*time.Minute) {
		t.Fatal("first SetIfAbsent must be true")
	}
	if c.SetIfAbsent("k", "v2", 5*time.Minute) {
		t.Fatal("second SetIfAbsent must be false")
	}
	if v, _ := c.Get("k"); v != "v1" {
		t.Fatalf("value must stay v1, got %q", v)
	}
	// ttl<=0：回 true 但不存入，也不影響同 key 既有 entry.
	if !c.SetIfAbsent("z", "v", 0) {
		t.Fatal("ttl<=0 must return true (fail-open)")
	}
	if _, ok := c.Get("z"); ok {
		t.Fatal("ttl<=0 must not store")
	}
	if !c.SetIfAbsent("neg", "v", -1) {
		t.Fatal("negative ttl must return true")
	}
	if c.SetIfAbsent("k", "v3", 0) != true {
		t.Fatal("ttl<=0 on existing key must still return true")
	}
	if v, _ := c.Get("k"); v != "v1" {
		t.Fatalf("ttl<=0 must not touch existing entry, got %q", v)
	}
	clk.Advance(5*time.Minute + time.Second)
	if !c.SetIfAbsent("k", "v4", 5*time.Minute) {
		t.Fatal("after expiry SetIfAbsent must be true")
	}
}

// [F24-12] 對應單號的測試.
func TestSetIfAbsentKeepsTTL(t *testing.T) {
	clk := newFakeClock()
	c := newNoEvict[string](testCfg("t", 0, clk), time.Hour, 100)
	c.SetIfAbsent("k", "v", 10*time.Second)
	clk.Advance(6 * time.Second)
	if c.SetIfAbsent("k", "v", 10*time.Second) {
		t.Fatal("control: second call must be false")
	}
	clk.Advance(5 * time.Second) // 距第一次 11s.
	if _, ok := c.Get("k"); ok {
		t.Fatal("failed SetIfAbsent must not extend TTL")
	}
}

// [F24-12] 對應單號的測試.
func TestSetIfAbsentConcurrent(t *testing.T) {
	clk := newFakeClock()
	c := newNoEvict[int](testCfg("t", 0, clk), time.Hour, 1000)
	const g = 200
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < g; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if c.SetIfAbsent("race", i, time.Minute) {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins.Load())
	}
}

// [F24-11] 對應單號的測試.
func TestDelete(t *testing.T) {
	clk := newFakeClock()
	c := newNoEvict[int](testCfg("t", 0, clk), time.Hour, 100)
	c.Set("a", 1)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("control: must hit before Delete")
	}
	c.Delete("a")
	if _, ok := c.Get("a"); ok {
		t.Fatal("must miss after Delete")
	}
	c.Delete("missing") // 不存在不得 panic.
	if !c.SetIfAbsent("a", 2, time.Minute) {
		t.Fatal("SetIfAbsent after Delete must be true (really deleted)")
	}
}

// [F24-11] 對應單號的測試.
func TestMaxSize(t *testing.T) {
	clk := newFakeClock()
	c := newPure[int](testCfg("t", 100, clk), time.Hour)
	for i := 0; i < 10000; i++ {
		c.Set(fmt.Sprintf("k%d", i), i)
	}
	c.cleanUp()
	if n := c.size(); n > 100 {
		t.Fatalf("size after CleanUp = %d, want <= 100", n)
	}
	if c.size() == 0 {
		t.Fatal("control: cache must not be empty")
	}
}

// [F24-11] 對應單號的測試.
func TestEvictionWarn(t *testing.T) {
	rec := captureLogs(t)
	clk := newFakeClock()
	c := newPure[int](testCfg("evict-me", 1, clk), time.Hour)
	c.Set("a", 1)
	c.cleanUp()
	if got := rec.warns("evicted"); len(got) != 0 {
		t.Fatalf("control: no Warn before overflow, got %v", got)
	}
	c.Set("b", 2)
	c.cleanUp()
	got := rec.warns("evicted")
	if len(got) != 1 {
		t.Fatalf("want exactly 1 eviction Warn, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "evict-me") {
		t.Fatalf("Warn must contain cache name: %q", got[0])
	}
	for i := 0; i < 100; i++ {
		c.Set(fmt.Sprintf("n%d", i), i)
	}
	c.cleanUp()
	if got := rec.warns("evicted"); len(got) != 1 {
		t.Fatalf("rate limit broken: %d Warns within a minute", len(got))
	}
	clk.Advance(61 * time.Second)
	for i := 0; i < 10; i++ {
		c.Set(fmt.Sprintf("m%d", i), i)
	}
	c.cleanUp()
	if got := rec.warns("evicted"); len(got) != 2 {
		t.Fatalf("after 61s want 2 Warns, got %d", len(got))
	}
}
