package geocoding

import "context"

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
