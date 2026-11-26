package geocoding

import (
	"context"
	"sync"
)

// gateKeyCtx is the unexported ctx key type for the per-user gate key.
type gateKeyCtx struct{}

// WithGateKey [F40-02] attaches a per-user gate key (the LINE userID) to ctx.
// The key lives only in ctx and the gate map; it must never be logged or put into error text.
func WithGateKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, gateKeyCtx{}, key)
}

// GateKeyFrom [F40-02] returns the per-user gate key carried by ctx, if any.
func GateKeyFrom(ctx context.Context) (string, bool) {
	key, ok := ctx.Value(gateKeyCtx{}).(string)
	return key, ok && key != ""
}

// userGate [F40-01] lets one key hold at most one slot at a time; others wait bounded only by their own ctx.
// It is process-local, which is correct for the single Render instance; running several instances
// would need a shared store instead (for example Redis SET NX per user).
// There is deliberately no cap on waiters per key (decision: queue, never reject): each waiter is one goroutine
// bounded by its caller's ctx (10s intent path, 2s bare path). The zero value is ready to use.
type userGate struct {
	mu sync.Mutex
	m  map[string]*gateEntry
}

// gateEntry is one live key: sem has capacity 1 (the slot), refs counts holder plus waiters.
type gateEntry struct {
	sem  chan struct{}
	refs int
}

var noopRelease = func() {}

// acquire takes the slot for key, waiting until it is free or ctx ends.
// An empty key means no gate. The returned release is idempotent and must be called once the slot is no longer needed.
// On ctx expiry the error is ctx.Err() and nothing is held.
func (u *userGate) acquire(ctx context.Context, key string) (release func(), err error) {
	if key == "" {
		return noopRelease, nil
	}
	u.mu.Lock()
	if u.m == nil {
		u.m = make(map[string]*gateEntry)
	}
	e, ok := u.m[key]
	if !ok {
		e = &gateEntry{sem: make(chan struct{}, 1)}
		u.m[key] = e
	}
	e.refs++
	u.mu.Unlock()

	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		u.drop(key, e, false)
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() { once.Do(func() { u.drop(key, e, true) }) }, nil
}

// drop gives back one reference; when held it also frees the slot. The entry is deleted at zero refs,
// so the map stays empty while idle. Everything happens under mu so it cannot race with get-or-create in acquire.
func (u *userGate) drop(key string, e *gateEntry, held bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if held {
		<-e.sem
	}
	e.refs--
	if e.refs == 0 {
		delete(u.m, key)
	}
}
