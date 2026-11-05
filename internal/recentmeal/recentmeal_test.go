package recentmeal

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// deadRedis 連到一定連不上的埠，所有操作回非 redis.Nil 的 error（repo 沒有 miniredis）.
func deadRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{
		Addr:          "127.0.0.1:1",
		MaxRetries:    -1,
		DialTimeout:   50 * time.Millisecond,
		DialerRetries: 1,
	})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// flakyHook go-redis Hook：down 為 true 時指令回 error，否則不呼叫 next 直接當作成功（不連網路）.
type flakyHook struct{ down atomic.Bool }

func (h *flakyHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *flakyHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.down.Load() {
			err := errors.New("redis down")
			cmd.SetErr(err)
			return err
		}
		return nil
	}
}

func (h *flakyHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if h.down.Load() {
			return errors.New("redis down")
		}
		return nil
	}
}

func TestMemoryStoreIsPerUserAndExpires(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	s := New(nil)
	s.Now = func() time.Time { return now }

	s.Remember(ctx, "U1", "m1", "text")
	if m, ok := s.Get(ctx, "U1"); !ok || m.ID != "m1" || m.Source != "text" || !m.At.Equal(now) {
		t.Fatalf("get = %+v %v", m, ok)
	}
	if _, ok := s.Get(ctx, "U2"); ok {
		t.Error("another user has no recent meal")
	}
	s.Now = func() time.Time { return now.Add(TTL + time.Second) }
	if _, ok := s.Get(ctx, "U1"); ok {
		t.Error("expired after the TTL")
	}
	s.Now = func() time.Time { return now }
	// [F24-47] 白箱：本地快取 TTL 未到、但 Meal.At 已超過 TTL 的項目不得被回傳（不靠推進時鐘，
	// 因為快取與 Get 共用同一個時鐘，快取會先到期而蓋掉這條檢查）.
	s.local.SetTTL("U9", Meal{ID: "m", At: now.Add(-TTL - time.Second)}, time.Hour)
	if _, ok := s.Get(ctx, "U9"); ok {
		t.Error("entry older than TTL must not be returned even if the cache entry is alive")
	}
	if err := s.Forget(ctx, "U1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(ctx, "U1"); ok {
		t.Error("forgotten")
	}
}

func TestNilStoreIsSafe(t *testing.T) {
	var s *Store
	s.Remember(context.Background(), "U1", "m1", "text")
	if _, ok := s.Get(context.Background(), "U1"); ok {
		t.Error("nil store remembers nothing")
	}
	if err := s.Forget(context.Background(), "U1"); err != nil {
		t.Error(err)
	}
}

// [F24-47] Redis 出錯時最近一餐走本地；Forget 回 Redis err 但本地已清.
func TestStoreFallsBackToLocalWhenRedisErrors(t *testing.T) {
	ctx := context.Background()
	s := New(deadRedis(t))
	s.Remember(ctx, "U1", "m1", "text")
	m, ok := s.Get(ctx, "U1")
	if !ok || m.ID != "m1" || m.Source != "text" {
		t.Fatalf("get = %+v %v", m, ok)
	}
	if _, ok := s.Get(ctx, "U2"); ok {
		t.Error("another user has no recent meal")
	}
	if err := s.Forget(ctx, "U1"); err == nil {
		t.Error("Forget should return the Redis error")
	}
	if _, ok := s.Get(ctx, "U1"); ok {
		t.Error("local entry must be cleared even though Redis failed")
	}
}

// [F24-47] 容量有上限：超量寫入不 panic、不死鎖；未滿時全部命中.
func TestStoreCapacityBounded(t *testing.T) {
	ctx := context.Background()

	over := New(nil)
	for i := 0; i < localCapacity+1000; i++ {
		id := "u" + strconv.Itoa(i)
		over.Remember(ctx, id, "m", "text")
		_, _ = over.Get(ctx, id) // 滿載後新 key 可能被拒收，只確認不 panic
	}
	// [F24-72] 上限斷言：字面 20000 筆相異 userID、寫入途中不 Get（otter 淘汰非同步），寫完計命中數，
	// 上限字面 10000（現行容量 5000 的兩倍寬限，吸收淘汰落後）。刻意釘死字面數字：容量常數誤改大時此處要紅；
	// 日後合法調大容量要同步改這裡.
	bounded := New(nil)
	for i := 0; i < 20000; i++ {
		bounded.Remember(ctx, "b"+strconv.Itoa(i), "m", "text")
	}
	hits := 0
	for i := 0; i < 20000; i++ {
		if _, ok := bounded.Get(ctx, "b"+strconv.Itoa(i)); ok {
			hits++
		}
	}
	if hits > 10000 {
		t.Errorf("local hits = %d after 20000 distinct users, want <= 10000", hits)
	}
	over.Remember(ctx, "last", "m", "text")
	if err := over.Forget(ctx, "last"); err != nil {
		t.Fatal(err)
	}
	if _, ok := over.Get(ctx, "last"); ok {
		t.Error("forgotten user must miss")
	}

	under := New(nil)
	for i := 0; i < 100; i++ {
		under.Remember(ctx, "u"+strconv.Itoa(i), "m", "text")
	}
	for i := 0; i < 100; i++ {
		if _, ok := under.Get(ctx, "u"+strconv.Itoa(i)); !ok {
			t.Errorf("u%d missing while far below capacity", i)
		}
	}
}

// [F24-71] Redis 壞 -> 好 -> 壞：好的那次寫入成功後本地舊值要清掉，第二次壞時不得讀回舊值.
func TestStoreClearsLocalAfterRedisRecovers(t *testing.T) {
	ctx := context.Background()
	hook := &flakyHook{}
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { _ = c.Close() })
	c.AddHook(hook)
	s := New(c)

	hook.down.Store(true)
	s.Remember(ctx, "U1", "old", "text") // 壞：寫進本地
	if m, ok := s.Get(ctx, "U1"); !ok || m.ID != "old" {
		t.Fatalf("local fallback should hold old, got %+v %v", m, ok)
	}
	hook.down.Store(false)
	s.Remember(ctx, "U1", "new", "text") // 好：Redis 寫入成功
	hook.down.Store(true)
	if m, ok := s.Get(ctx, "U1"); ok {
		t.Errorf("stale local value must not be read back after Redis recovered, got %+v", m)
	}
}
