package ai

import (
	"context"
	"testing"
	"time"
)

// remainingProvider 記下每次呼叫拿到的 ctx 剩餘時間；hook 可在呼叫時推進假時鐘.
type remainingProvider struct {
	*funcProvider
	remaining []time.Duration
	hook      func(call int)
}

func newRemainingProvider(name string, models ...string) *remainingProvider {
	rp := &remainingProvider{}
	rp.funcProvider = &funcProvider{name: name, models: models, fn: func(ctx context.Context, _ string, _ *Request) (string, error) {
		d, ok := ctx.Deadline()
		if !ok {
			return "", context.Canceled
		}
		rp.remaining = append(rp.remaining, time.Until(d))
		if rp.hook != nil {
			rp.hook(len(rp.remaining))
		}
		if len(rp.remaining) < len(models) && rp.hook != nil {
			return "", &APIError{Status: 400} // 非供應商故障：只用來讓路由繼續試下一個模型.
		}
		return "ok", nil
	}}
	return rp
}

// remainingClaude 帶 Limits() 的版本（像 *Claude）.
type remainingClaude struct{ *remainingProvider }

func (remainingClaude) Limits() (attempt, budget time.Duration) {
	return 25 * time.Second, 25 * time.Second
}

func TestAttemptTimeoutCapsSingleCall(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings Settings
		max      time.Duration
	}{
		{"default 8s", Settings{}, 8 * time.Second},
		{"explicit 3s", Settings{AttemptTimeout: 3 * time.Second}, 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.settings.TextOrder = []string{"groq"}
			p := newRemainingProvider("groq", "m")
			r := NewRouter(&tc.settings, p)

			if res := r.Complete(context.Background(), textReq()); !res.OK() {
				t.Fatalf("%+v", res)
			}
			got := p.remaining[0]
			if got <= 0 || got > tc.max {
				t.Errorf("[F33-03] call got %v, want within (0, %v]", got, tc.max)
			}
			if got >= 10*time.Second {
				t.Errorf("[F33-03] call got the whole provider budget (%v)", got)
			}
		})
	}
}

func TestAttemptTimeoutShrinksToRemainingBudget(t *testing.T) {
	p := newRemainingProvider("groq", "a", "b")
	r, clock := newTestRouter(&Settings{TextOrder: []string{"groq"}}, p)
	p.hook = func(call int) {
		if call == 1 {
			clock.t = clock.t.Add(9 * time.Second) // 第一次用掉 10s 預算中的 9s.
		}
	}

	if res := r.Complete(context.Background(), textReq()); !res.OK() || len(p.remaining) != 2 {
		t.Fatalf("res = %+v remaining = %v", res, p.remaining)
	}
	if got := p.remaining[1]; got <= 0 || got > time.Second {
		t.Errorf("[F33-03] second call got %v, want at most the 1s left in the budget (not the full 8s)", got)
	}
}

func TestAttemptTimeoutDoesNotConsumeWholeBudget(t *testing.T) {
	p := &funcProvider{name: "groq", models: []string{"a", "b"}, fn: func(ctx context.Context, model string, _ *Request) (string, error) {
		if model == "a" {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "ok", nil
	}}
	r := NewRouter(&Settings{TextOrder: []string{"groq"}, AttemptTimeout: 50 * time.Millisecond}, p)

	res := r.Complete(context.Background(), textReq())

	if !res.OK() || res.Model != "b" {
		t.Errorf("[F33-03] a timed-out model must not eat the whole budget: %+v", res)
	}
	if got := p.callCount(); got != 2 {
		t.Errorf("calls = %v, want both models in the same Complete", p.calls)
	}
}

func TestCallerDeadlineStillWinsOverAttemptTimeout(t *testing.T) {
	for _, tc := range []struct {
		name     string
		callerIn time.Duration
		max      time.Duration
		min      time.Duration
	}{
		{"caller 2s", 2 * time.Second, 2 * time.Second, 0},
		{"caller 20s", 20 * time.Second, 8 * time.Second, 2 * time.Second}, // min 是正對照：不是被誤砍成很小.
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newRemainingProvider("groq", "m")
			r := NewRouter(&Settings{TextOrder: []string{"groq"}}, p)
			ctx, cancel := context.WithTimeout(context.Background(), tc.callerIn)
			defer cancel()

			r.Complete(ctx, textReq())

			if got := p.remaining[0]; got <= tc.min || got > tc.max {
				t.Errorf("[F33-03] call got %v, want within (%v, %v]", got, tc.min, tc.max)
			}
		})
	}
}

func TestSettingsDefaults(t *testing.T) {
	got := NewRouter(&Settings{}).settings
	if got.AttemptTimeout != 8*time.Second || got.ProviderBudget != 10*time.Second ||
		got.ProviderCooldown != 60*time.Second || got.RateLimitWait != 0 {
		t.Errorf("[F33-06] defaults = attempt %v budget %v cooldown %v rateWait %v", got.AttemptTimeout, got.ProviderBudget, got.ProviderCooldown, got.RateLimitWait)
	}

	neg := NewRouter(&Settings{AttemptTimeout: -1, ProviderBudget: -1, ProviderCooldown: -1, RateLimitWait: -5 * time.Second}).settings
	if neg.AttemptTimeout != 8*time.Second || neg.ProviderBudget != 10*time.Second || neg.ProviderCooldown != 60*time.Second || neg.RateLimitWait != 0 {
		t.Errorf("[F33-06] negatives = %+v", neg)
	}

	set := NewRouter(&Settings{AttemptTimeout: 3 * time.Second, ProviderCooldown: 5 * time.Second, RateLimitWait: 8 * time.Second}).settings
	if set.AttemptTimeout != 3*time.Second || set.ProviderCooldown != 5*time.Second || set.RateLimitWait != 8*time.Second {
		t.Errorf("[F33-06] explicit values must be kept: %+v", set)
	}
}

func TestClaudeKeepsLongLimits(t *testing.T) {
	claude := remainingClaude{newRemainingProvider("claude", "opus")}
	r := NewRouter(&Settings{AttemptTimeout: 3 * time.Second, ProviderBudget: 4 * time.Second}, claude)

	r.Complete(context.Background(), &Request{Prompt: "p", Order: []string{"claude"}})

	if got := claude.remaining[0]; got <= 8*time.Second || got > 25*time.Second {
		t.Errorf("[F33-07] claude call got %v, want within (8s, 25s]; Settings must not apply to it", got)
	}

	built, ok := NewClaude("k", "").(*Claude)
	if !ok {
		t.Fatal("NewClaude did not return *Claude")
	}
	var lp limitsProvider = built
	if attempt, budget := lp.Limits(); attempt != 25*time.Second || budget != 25*time.Second {
		t.Errorf("[F33-07] *Claude.Limits() = %v/%v, want 25s/25s", attempt, budget)
	}
}

func TestOtherProvidersCappedAt8sWhileClaudeIsNot(t *testing.T) {
	claude := remainingClaude{newRemainingProvider("claude", "opus1", "opus2")}
	others := []*remainingProvider{
		newRemainingProvider("groq", "g1", "g2"), newRemainingProvider("nvidia", "n1", "n2"), newRemainingProvider("gemini", "m1", "m2"),
	}
	r, clock := newTestRouter(&Settings{}, claude, others[0], others[1], others[2])
	hook := func(call int) {
		if call == 1 {
			clock.t = clock.t.Add(9 * time.Second)
		}
	}
	claude.hook = hook
	for _, o := range others {
		o.hook = hook
	}

	r.Complete(context.Background(), &Request{Prompt: "p", Order: []string{"claude"}})
	if got := claude.remaining[1]; got <= 8*time.Second || got > 16*time.Second {
		t.Errorf("[F33-07] claude after 9s used: second call got %v, want within (8s, 16s] (its own 25s budget minus 9s)", got)
	}

	for _, o := range others {
		r.Complete(context.Background(), &Request{Prompt: "p", Order: []string{o.Name()}})
		if got := o.remaining[0]; got <= 0 || got > 8*time.Second {
			t.Errorf("[F33-03] %s first call got %v, want ≤8s", o.Name(), got)
		}
		if got := o.remaining[1]; got <= 0 || got > time.Second {
			t.Errorf("[F33-03] %s second call got %v, want ≤1s of the 10s budget", o.Name(), got)
		}
	}
}

func TestClaudeLimitsStillCappedByCallerDeadline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caller time.Duration
	}{{"caller 3s", 3 * time.Second}, {"caller 30s", 30 * time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			claude := remainingClaude{newRemainingProvider("claude", "opus")}
			groq := newRemainingProvider("groq", "g")
			r := NewRouter(&Settings{}, claude, groq)
			ctx, cancel := context.WithTimeout(context.Background(), tc.caller)
			defer cancel()

			r.Complete(ctx, &Request{Prompt: "p", Order: []string{"claude"}})
			r.Complete(ctx, &Request{Prompt: "p", Order: []string{"groq"}})

			c, g := claude.remaining[0], groq.remaining[0]
			if tc.caller == 3*time.Second {
				if c <= 0 || c > 3*time.Second || g <= 0 || g > 3*time.Second {
					t.Errorf("[F33-07] caller 3s: claude %v groq %v must both be ≤3s", c, g)
				}
				return
			}
			if c <= 8*time.Second || c > 25*time.Second {
				t.Errorf("[F33-07] claude %v, want >8s when the caller allows 30s", c)
			}
			if g <= 0 || g > 8*time.Second {
				t.Errorf("[F33-03] groq %v, want ≤8s", g)
			}
		})
	}
}
