package conversation

import (
	"chatbot-go/internal/platform/localcache"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock 可前進的假時鐘.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// flakyStore 可切換成一律失敗的假 Redis 後端.
type flakyStore struct {
	inner cacheStore
	fail  bool
}

// newCacheStore 以 localcache 建立假 Redis 後端（[F24-31]）.
func newCacheStore(now func() time.Time) cacheStore {
	return cacheStore{c: localcache.NewWithClock[string]("conversation_test_fake", memoryBackendMax, stateTTL, now)}
}

var errRedisDown = errors.New("redis down")

func (f *flakyStore) get(ctx context.Context, key string) (value string, found bool, err error) {
	if f.fail {
		return "", false, errRedisDown
	}
	return f.inner.get(ctx, key)
}

func (f *flakyStore) set(ctx context.Context, key, value string, ttl time.Duration) error {
	if f.fail {
		return errRedisDown
	}
	return f.inner.set(ctx, key, value, ttl)
}

func (f *flakyStore) del(ctx context.Context, key string) error {
	if f.fail {
		return errRedisDown
	}
	return f.inner.del(ctx, key)
}

// TestManagerWithoutRedisKeepsState [F24-01].
func TestManagerWithoutRedisKeepsState(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil)
	if !m.Available() {
		t.Fatal("Available() = false, want true without Redis")
	}
	want := []string{"臺北市", "新北市"}
	if err := m.Set(ctx, "U1", &State{Type: TypeWeatherRegionSelect, Regions: want}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := m.Get(ctx, "U1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil, want stored state")
	}
	if got.Type != TypeWeatherRegionSelect {
		t.Errorf("Type = %q, want %q", got.Type, TypeWeatherRegionSelect)
	}
	if strings.Join(got.Regions, ",") != "臺北市,新北市" {
		t.Errorf("Regions = %v", got.Regions)
	}

	other, err := m.Get(ctx, "U2")
	if err != nil || other != nil {
		t.Errorf("Get unknown user = %v, %v; want nil, nil", other, err)
	}
}

// TestMemoryStateExpiresAfterTTL [F24-01].
func TestMemoryStateExpiresAfterTTL(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()
	m := newManager(nil, clk.now)

	if err := m.Set(ctx, "U1", &State{Type: TypeLocationClarify}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	clk.advance(stateTTL - time.Second)
	if got, _ := m.Get(ctx, "U1"); got == nil {
		t.Fatal("state expired before TTL")
	}
	clk.advance(2 * time.Second)
	if got, _ := m.Get(ctx, "U1"); got != nil {
		t.Fatalf("state still present after TTL: %+v", got)
	}
}

// TestRedisFailureFallsBackToMemory [F24-02].
func TestRedisFailureFallsBackToMemory(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil)
	m.backend = &flakyStore{inner: newCacheStore(time.Now), fail: true}

	if err := m.Set(ctx, "U1", &State{Type: TypeLocationClarify}); err != nil {
		t.Fatalf("Set should fall back, got %v", err)
	}
	got, err := m.Get(ctx, "U1")
	if err != nil {
		t.Fatalf("Get should fall back, got %v", err)
	}
	if got == nil || got.Type != TypeLocationClarify {
		t.Fatalf("Get = %+v, want TypeLocationClarify", got)
	}
	if err := m.Delete(ctx, "U1"); err == nil {
		t.Error("Delete should return Redis error")
	}
	got, err = m.Get(ctx, "U1")
	if err != nil || got != nil {
		t.Errorf("Get after Delete = %+v, %v; want nil, nil", got, err)
	}
}

// TestRedisRecoveryClearsMemoryCopy [F24-02].
func TestRedisRecoveryClearsMemoryCopy(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil)
	flaky := &flakyStore{inner: newCacheStore(time.Now), fail: true}
	m.backend = flaky

	if err := m.Set(ctx, "U1", &State{Type: TypeWeatherRegionSelect}); err != nil {
		t.Fatalf("Set A: %v", err)
	}
	flaky.fail = false
	if err := m.Set(ctx, "U1", &State{Type: TypeLocationClarify}); err != nil {
		t.Fatalf("Set B: %v", err)
	}
	// 模擬 Redis 那份過期
	if err := flaky.inner.del(ctx, keyPrefix+"U1"); err != nil {
		t.Fatalf("del: %v", err)
	}
	got, err := m.Get(ctx, "U1")
	if err != nil || got != nil {
		t.Errorf("Get = %+v, %v; want nil (stale memory copy must be cleared)", got, err)
	}
}

// TestRedisHealthyPathUsesRedis [F24-02].
func TestRedisHealthyPathUsesRedis(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil)
	flaky := &flakyStore{inner: newCacheStore(time.Now), fail: false}
	m.backend = flaky

	if err := m.Set(ctx, "U1", &State{Type: TypeLocationClarify}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, found, _ := flaky.inner.get(ctx, keyPrefix+"U1"); !found {
		t.Error("state not written to Redis backend")
	}
	if _, found := m.local.Get(keyPrefix + "U1"); found {
		t.Error("state should not be in memory when Redis is healthy")
	}
	got, err := m.Get(ctx, "U1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.Type != TypeLocationClarify {
		t.Fatalf("Get = %+v, want TypeLocationClarify", got)
	}
}

func newFlakyManager() (*Manager, *flakyStore) {
	return newFlakyManagerWithClock(time.Now)
}

func newFlakyManagerWithClock(now func() time.Time) (*Manager, *flakyStore) {
	f := &flakyStore{inner: newCacheStore(time.Now)}
	return newManager(f, now), f
}

// TestGetPrefersLocalOverRedisAfterFailedSet [F24-04].
func TestGetPrefersLocalOverRedisAfterFailedSet(t *testing.T) {
	ctx := context.Background()
	m, f := newFlakyManager()
	if err := m.Set(ctx, "U1", &State{Type: TypeWeatherRegionSelect}); err != nil {
		t.Fatalf("Set S1: %v", err)
	}
	f.fail = true
	if err := m.Set(ctx, "U1", &State{Type: TypeLocationClarify}); err != nil {
		t.Fatalf("Set S2: %v", err)
	}
	f.fail = false
	got, err := m.Get(ctx, "U1")
	if err != nil || got == nil || got.Type != TypeLocationClarify {
		t.Fatalf("Get = %+v, %v; want TypeLocationClarify", got, err)
	}
}

// TestDeleteWhileRedisDownLeavesTombstone [F24-04].
func TestDeleteWhileRedisDownLeavesTombstone(t *testing.T) {
	ctx := context.Background()
	m, f := newFlakyManager()
	if err := m.Set(ctx, "U1", &State{Type: TypeWeatherRegionSelect}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	f.fail = true
	if err := m.Delete(ctx, "U1"); err == nil {
		t.Fatal("Delete should return Redis error")
	}
	f.fail = false
	got, err := m.Get(ctx, "U1")
	if err != nil || got != nil {
		t.Fatalf("Get = %+v, %v; want nil, nil (tombstone)", got, err)
	}
}

// TestFailedSetKeepsTombstoneAfterLocalLoss [F24-61] Redis 寫入失敗時新狀態只在本地；
// 本地項消失後不得讀回 Redis 裡使用者已刪除的舊狀態.
func TestFailedSetKeepsTombstoneAfterLocalLoss(t *testing.T) {
	ctx := context.Background()
	m, f := newFlakyManager()
	if err := m.Set(ctx, "U1", &State{Type: TypeLocationClarify}); err != nil {
		t.Fatalf("Set old state: %v", err)
	}
	f.fail = true
	if err := m.Delete(ctx, "U1"); err == nil {
		t.Fatal("Delete should return Redis error")
	}
	if err := m.Set(ctx, "U1", &State{Type: TypeWeatherRegionSelect}); err != nil {
		t.Fatalf("Set new state: %v", err)
	}
	// 模擬本地項被容量淘汰：otter 滿載時淘汰誰不固定，灌資料無法穩定重現.
	m.local.Delete(keyPrefix + "U1")
	f.fail = false
	got, err := m.Get(ctx, "U1")
	if err != nil || got != nil {
		t.Fatalf("Get = %+v, %v; want nil, nil (deleted state must not come back)", got, err)
	}
}

// TestRedisSetClearsTombstone [F24-04].
func TestRedisSetClearsTombstone(t *testing.T) {
	ctx := context.Background()
	m, f := newFlakyManager()
	_ = m.Set(ctx, "U1", &State{Type: TypeWeatherRegionSelect})
	f.fail = true
	_ = m.Delete(ctx, "U1")
	f.fail = false
	if err := m.Set(ctx, "U1", &State{Type: TypeWorkoutConfirm}); err != nil {
		t.Fatalf("Set S3: %v", err)
	}
	got, err := m.Get(ctx, "U1")
	if err != nil || got == nil || got.Type != TypeWorkoutConfirm {
		t.Fatalf("Get = %+v, %v; want TypeWorkoutConfirm", got, err)
	}
}

// TestTombstoneExpires [F24-04].
func TestTombstoneExpires(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()
	m, f := newFlakyManagerWithClock(clk.now)
	_ = m.Set(ctx, "U1", &State{Type: TypeWeatherRegionSelect})
	f.fail = true
	_ = m.Delete(ctx, "U1")
	f.fail = false
	if got, _ := m.Get(ctx, "U1"); got != nil {
		t.Fatalf("tombstone should hide Redis value, got %+v", got)
	}
	clk.advance(stateTTL + time.Second)
	got, err := m.Get(ctx, "U1")
	if err != nil || got == nil || got.Type != TypeWeatherRegionSelect {
		t.Fatalf("Get after tombstone expiry = %+v, %v; want Redis value", got, err)
	}
}

// TestDeleteConfirmRejectedWithoutRedis [F24-05].
func TestDeleteConfirmRejectedWithoutRedis(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil)
	if err := m.Set(ctx, "U1", &State{Type: TypeDeleteDataConfirm}); err == nil {
		t.Fatal("Set delete confirm without Redis should error")
	}
	if got, err := m.Get(ctx, "U1"); err != nil || got != nil {
		t.Fatalf("Get = %+v, %v; want nil, nil", got, err)
	}
}

// TestDeleteConfirmRejectedWhenRedisSetFails [F24-05].
func TestDeleteConfirmRejectedWhenRedisSetFails(t *testing.T) {
	ctx := context.Background()
	m, f := newFlakyManager()
	f.fail = true
	if err := m.Set(ctx, "U1", &State{Type: TypeDeleteDataConfirm}); err == nil {
		t.Fatal("Set delete confirm with Redis failure should error")
	}
	if _, found := m.local.Get(keyPrefix + "U1"); found {
		t.Errorf("local must hold nothing, found=%v", found)
	}
	f.fail = false
	if got, err := m.Get(ctx, "U1"); err != nil || got != nil {
		t.Fatalf("Get = %+v, %v; want nil, nil", got, err)
	}
}

// TestOtherStatesStillFallBack [F24-05].
func TestOtherStatesStillFallBack(t *testing.T) {
	for _, typ := range []StateType{TypeWorkoutConfirm, TypeLocationClarify} {
		t.Run(string(typ), func(t *testing.T) {
			ctx := context.Background()
			m, f := newFlakyManager()
			f.fail = true
			if err := m.Set(ctx, "U1", &State{Type: typ}); err != nil {
				t.Fatalf("Set: %v", err)
			}
			got, err := m.Get(ctx, "U1")
			if err != nil || got == nil || got.Type != typ {
				t.Fatalf("Get = %+v, %v; want %s", got, err, typ)
			}
		})
	}
}

// TestDeleteConfirmStoredInRedis [F24-05] 正常路徑：Redis 可用時刪除確認存得進去.
func TestDeleteConfirmStoredInRedis(t *testing.T) {
	ctx := context.Background()
	m, _ := newFlakyManager()
	if err := m.Set(ctx, "U1", &State{Type: TypeDeleteDataConfirm}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := m.Get(ctx, "U1")
	if err != nil || got == nil || got.Type != TypeDeleteDataConfirm {
		t.Fatalf("Get = %+v, %v", got, err)
	}
}

// TestTombstoneSurvivesLocalCacheFull [F24-61] 本地狀態快取被灌滿後，刪除標記仍擋得住 Redis 裡的舊刪除確認.
func TestTombstoneSurvivesLocalCacheFull(t *testing.T) {
	ctx := context.Background()
	m, f := newFlakyManager()
	if err := m.Set(ctx, "victim", &State{Type: TypeDeleteDataConfirm}); err != nil {
		t.Fatalf("Set delete confirm: %v", err)
	}
	f.fail = true
	if err := m.Delete(ctx, "victim"); err == nil {
		t.Fatal("Delete should return Redis error")
	}
	// Redis 故障期間，大量其他使用者把本地狀態快取灌到 2 倍容量
	for i := 0; i < 2*maxLocalStates; i++ {
		if err := m.Set(ctx, fmt.Sprintf("other-%d", i), &State{Type: TypeLocationClarify}); err != nil {
			t.Fatalf("Set other-%d: %v", i, err)
		}
	}
	f.fail = false
	got, err := m.Get(ctx, "victim")
	if err != nil || got != nil {
		t.Fatalf("Get = %+v, %v; want nil, nil (tombstone must survive a full local cache)", got, err)
	}
}

// TestLocalStateCapacityKeepsConcurrentUsers [F24-62] Redis 故障時，本地備援至少保住 100 位使用者的狀態.
// 上限由 TestLocalStateCapacityIsBounded 守；這裡只守下限.
func TestLocalStateCapacityKeepsConcurrentUsers(t *testing.T) {
	ctx := context.Background()
	m, f := newFlakyManager()
	f.fail = true
	const users = 100
	for i := 0; i < users; i++ {
		id := fmt.Sprintf("user-%d", i)
		if err := m.Set(ctx, id, &State{Type: TypeWeatherRegionSelect, Regions: []string{id}}); err != nil {
			t.Fatalf("Set %s: %v", id, err)
		}
	}
	for i := 0; i < users; i++ {
		id := fmt.Sprintf("user-%d", i)
		got, err := m.Get(ctx, id)
		if err != nil || got == nil {
			t.Fatalf("Get %s = %+v, %v; want own state", id, got, err)
		}
		if len(got.Regions) != 1 || got.Regions[0] != id {
			t.Fatalf("Get %s Regions = %v, want [%s]", id, got.Regions, id)
		}
	}
}

// TestLocalStateCapacityIsBounded [F241-02] Redis 長時間故障時，本地備援筆數有上限.
// 字面 20000 位相異使用者、寫入途中不 Get（otter 淘汰非同步），寫完計命中數；
// 上限字面 2000（現行容量 1000 的兩倍寬限，吸收淘汰落後）。刻意不引用 maxLocalStates：
// 常數被誤改大時此處要紅；日後合法調大容量要同步改這裡.
func TestLocalStateCapacityIsBounded(t *testing.T) {
	ctx := context.Background()
	m, f := newFlakyManager()
	f.fail = true
	const users = 20000
	for i := 0; i < users; i++ {
		id := fmt.Sprintf("cap-%d", i)
		if err := m.Set(ctx, id, &State{Type: TypeWeatherRegionSelect, Regions: []string{id}}); err != nil {
			t.Fatalf("Set %s: %v", id, err)
		}
	}
	hits := 0
	for i := 0; i < users; i++ {
		got, err := m.Get(ctx, fmt.Sprintf("cap-%d", i))
		if err != nil {
			t.Fatalf("Get cap-%d: %v", i, err)
		}
		if got != nil {
			hits++
		}
	}
	if hits > 2000 {
		t.Errorf("local hits = %d after %d distinct users, want <= 2000", hits, users)
	}
}

// TestGetStrictReturnsReadError [F24-63] Redis 讀取失敗時 GetStrict 回 error，Get 仍回 nil, nil.
func TestGetStrictReturnsReadError(t *testing.T) {
	ctx := context.Background()
	m, f := newFlakyManager()
	if err := m.Set(ctx, "U1", &State{Type: TypeDeleteDataConfirm}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	f.fail = true
	got, err := m.GetStrict(ctx, "U1")
	if err == nil || !errors.Is(err, ErrStateRead) {
		t.Fatalf("GetStrict = %+v, %v; want ErrStateRead", got, err)
	}
	if got != nil {
		t.Errorf("GetStrict state = %+v, want nil on read error", got)
	}
	got, err = m.Get(ctx, "U1")
	if err != nil || got != nil {
		t.Errorf("Get = %+v, %v; want nil, nil (read error swallowed)", got, err)
	}
	f.fail = false
	got, err = m.GetStrict(ctx, "U1")
	if err != nil || got == nil || got.Type != TypeDeleteDataConfirm {
		t.Fatalf("GetStrict after recovery = %+v, %v; want TypeDeleteDataConfirm", got, err)
	}
}

// TestGetStrictDecodeErrorIsNotReadError [F24-63] 解碼失敗不是讀取錯誤，Get 與 GetStrict 都原樣回傳.
func TestGetStrictDecodeErrorIsNotReadError(t *testing.T) {
	ctx := context.Background()
	m, f := newFlakyManager()
	if err := f.inner.set(ctx, keyPrefix+"U1", "{not json", stateTTL); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for name, get := range map[string]func(context.Context, string) (*State, error){"GetStrict": m.GetStrict, "Get": m.Get} {
		got, err := get(ctx, "U1")
		if err == nil || errors.Is(err, ErrStateRead) || got != nil {
			t.Errorf("%s = %+v, %v; want decode error that is not ErrStateRead", name, got, err)
		}
	}
}
