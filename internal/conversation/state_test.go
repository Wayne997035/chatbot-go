package conversation

import (
	"context"
	"errors"
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
	inner *memoryStore
	fail  bool
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
	m := NewManager(nil)
	clk := newFakeClock()
	m.local.now = clk.now

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

// TestMemoryStorePrunesExpiredWhenFull [F24-01].
func TestMemoryStorePrunesExpiredWhenFull(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore()
	clk := newFakeClock()
	s.now = clk.now

	for i := 0; i < maxLocalStates; i++ {
		key := keyPrefix + "U" + string(rune('A'+i%26)) + strings.Repeat("x", i/26)
		if err := s.set(ctx, key, "v", time.Minute); err != nil {
			t.Fatalf("set: %v", err)
		}
	}
	if len(s.data) != maxLocalStates {
		t.Fatalf("len = %d, want %d", len(s.data), maxLocalStates)
	}
	clk.advance(2 * time.Minute)
	if err := s.set(ctx, keyPrefix+"new", "v", time.Minute); err != nil {
		t.Fatalf("set: %v", err)
	}
	if len(s.data) != 1 {
		t.Errorf("len = %d after prune, want 1", len(s.data))
	}
}

// TestRedisFailureFallsBackToMemory [F24-02].
func TestRedisFailureFallsBackToMemory(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil)
	m.backend = &flakyStore{inner: newMemoryStore(), fail: true}

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
	flaky := &flakyStore{inner: newMemoryStore(), fail: true}
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
	flaky := &flakyStore{inner: newMemoryStore(), fail: false}
	m.backend = flaky

	if err := m.Set(ctx, "U1", &State{Type: TypeLocationClarify}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, found, _ := flaky.inner.get(ctx, keyPrefix+"U1"); !found {
		t.Error("state not written to Redis backend")
	}
	if _, found, _ := m.local.get(ctx, keyPrefix+"U1"); found {
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
