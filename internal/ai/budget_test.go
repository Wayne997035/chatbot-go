package ai

import (
	"context"
	"testing"
)

func budgetRouter() (*Router, *fakeProvider) {
	p := &fakeProvider{name: "groq", models: textModels("m1"), script: okScript()}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq"}}, p)
	return r, p
}

// [F36-01] B1：同一則訊息連叫 4 次（N,N,F,F）：前三次放行，第四次跳過且不打供應商.
func TestBudgetCapsCompleteCallsPerMessage(t *testing.T) {
	r, p := budgetRouter()
	ctx := EnsureBudget(context.Background())

	steps := []struct {
		final       bool
		wantSkipped bool
	}{{false, false}, {false, false}, {true, false}, {true, true}}
	for i, s := range steps {
		before := len(p.calls)
		res := r.Complete(ctx, &Request{Prompt: "p", Final: s.final})
		if res.Skipped != s.wantSkipped {
			t.Fatalf("call %d: Skipped = %v, want %v", i+1, res.Skipped, s.wantSkipped)
		}
		if s.wantSkipped {
			if res.OK() {
				t.Errorf("call %d: a skipped call must not be OK", i+1)
			}
			if len(p.calls) != before {
				t.Errorf("call %d: provider was hit although the budget was spent (calls=%v)", i+1, p.calls)
			}
			continue
		}
		if res.Attempts == 0 || !res.OK() {
			t.Errorf("call %d: want an attempt that succeeded, got %+v", i+1, res)
		}
	}
	if len(p.calls) != MessageCalls {
		t.Errorf("provider calls = %d, want %d", len(p.calls), MessageCalls)
	}
}

// [F36-01] B1 負面：ctx 沒有 Budget（排程、直呼 Router）連叫 10 次全放行.
func TestBudgetAbsentIsUnlimited(t *testing.T) {
	r, p := budgetRouter()
	for i := range 10 {
		res := r.Complete(context.Background(), &Request{Prompt: "p"})
		if res.Skipped || !res.OK() {
			t.Fatalf("call %d skipped without a budget: %+v", i+1, res)
		}
	}
	if len(p.calls) != 10 {
		t.Errorf("provider calls = %d, want 10", len(p.calls))
	}
	if !CanSpend(context.Background(), false) || !CanSpend(context.Background(), true) {
		t.Error("CanSpend must be true when ctx has no Budget")
	}
}

// [F36-01] B2：問答之前最多 2 次；問答保底 1 次；EnsureBudget 冪等.
func TestBudgetReservesOneCallForFinal(t *testing.T) {
	r, p := budgetRouter()
	ctx := EnsureBudget(context.Background())
	if again := EnsureBudget(ctx); again != ctx {
		t.Fatal("EnsureBudget must return the same ctx when a Budget is already installed")
	}

	got := []bool{}
	for _, final := range []bool{false, false, false, true, true} {
		got = append(got, r.Complete(ctx, &Request{Prompt: "p", Final: final}).Skipped)
	}
	want := []bool{false, false, true, false, true}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Skipped sequence = %v, want %v (N,N,N,F,F)", got, want)
		}
	}
	if len(p.calls) != 3 {
		t.Errorf("provider calls = %d, want 3", len(p.calls))
	}
	if CanSpend(ctx, true) {
		t.Error("CanSpend(final) must be false once 3 calls are used")
	}
}

// [F36-01] 一次 Complete 不論試幾個模型只扣 1 次額度.
func TestBudgetChargesOncePerCompleteNotPerModel(t *testing.T) {
	p := &fakeProvider{name: "groq", models: textModels("m1", "m2", "m3"), script: failScript()}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq"}}, p)
	ctx := EnsureBudget(context.Background())

	res := r.Complete(ctx, &Request{Prompt: "p"})
	if res.Attempts != 3 || res.OK() {
		t.Fatalf("want 3 failed attempts, got %+v", res)
	}
	if !CanSpend(ctx, false) {
		t.Error("one failed Complete (3 models) must charge only one call, a second pre-step must still fit")
	}
}
