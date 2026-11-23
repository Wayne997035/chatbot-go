package ai

import (
	"context"
	"sync"
)

// [F36-01] 每則 user 訊息的 AI 額度：最多 MessageCalls 次 Router.Complete，
// 問答（Final）之前的步驟合計最多 PreFinalCalls 次，保證問答至少有 1 次.
const (
	// MessageCalls 一則訊息最多的 Router.Complete 次數.
	MessageCalls = 3
	// PreFinalCalls 問答之前的步驟（規劃器、記餐、運動、意圖）合計最多的次數.
	PreFinalCalls = 2
)

// Budget 一則訊息的 AI 額度計數器；隨 ctx 傳遞，併發安全.
type Budget struct {
	mu     sync.Mutex
	used   int
	usedPr int
}

type budgetKey struct{}

// EnsureBudget 確保 ctx 帶有一份額度；冪等：ctx 已有就原樣回傳（同一則訊息只有一份）.
func EnsureBudget(ctx context.Context) context.Context {
	if _, ok := ctx.Value(budgetKey{}).(*Budget); ok {
		return ctx
	}
	return context.WithValue(ctx, budgetKey{}, &Budget{})
}

func budgetOf(ctx context.Context) *Budget {
	b, _ := ctx.Value(budgetKey{}).(*Budget)
	return b
}

// allowed 呼叫端須持有 mu.
func (b *Budget) allowed(final bool) bool {
	if b.used >= MessageCalls {
		return false
	}
	return final || b.usedPr < PreFinalCalls
}

// take 嘗試扣一次；扣不到回 false 且不改計數.
func (b *Budget) take(final bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.allowed(final) {
		return false
	}
	b.used++
	if !final {
		b.usedPr++
	}
	return true
}

// CanSpend 還扣得到一次嗎（不扣）；ctx 沒有 Budget（排程、直呼 Router）= 不限，回 true.
//
// 前置步驟在扣每日計數之前先問它，額度用完就靜默跳過.
func CanSpend(ctx context.Context, final bool) bool {
	b := budgetOf(ctx)
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.allowed(final)
}

// Take 扣一次額度；ctx 沒有 Budget = 不限，回 true. Router.Complete 的唯一入口使用.
func Take(ctx context.Context, final bool) bool {
	b := budgetOf(ctx)
	if b == nil {
		return true
	}
	return b.take(final)
}
