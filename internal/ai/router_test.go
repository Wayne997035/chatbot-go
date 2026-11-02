package ai

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeProvider 依腳本回傳結果的假供應商.
type fakeProvider struct {
	name   string
	models map[Capability][]string
	// script 依呼叫順序回傳的結果；用完後一律回傳最後一個.
	script []fakeReply
	calls  []string
}

type fakeReply struct {
	text string
	err  error
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Candidates(_ context.Context, c Capability) ([]string, error) {
	return f.models[c], nil
}

func (f *fakeProvider) Call(_ context.Context, model string, _ *Request) (string, error) {
	f.calls = append(f.calls, model)
	i := len(f.calls) - 1
	if i >= len(f.script) {
		i = len(f.script) - 1
	}
	return f.script[i].text, f.script[i].err
}

func textModels(models ...string) map[Capability][]string {
	return map[Capability][]string{Text: models}
}

// fakeClock 可控時間，sleep 只推進時間並記錄.
type fakeClock struct {
	t      time.Time
	sleeps []time.Duration
}

func newTestRouter(settings *Settings, providers ...Provider) (*Router, *fakeClock) {
	r := NewRouter(settings, providers...)
	clock := &fakeClock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	r.now = func() time.Time { return clock.t }
	r.sleep = func(_ context.Context, d time.Duration) error {
		clock.sleeps = append(clock.sleeps, d)
		clock.t = clock.t.Add(d)
		return nil
	}
	return r, clock
}

const validJSON = `{"summary":"ok"}`

func isJSON(text string) bool { return strings.HasPrefix(text, "{") }

// 真實錯誤樣本（ID 已遮蔽），取自 intelligence-flow 2026-10-08 的失敗排程.
var (
	groqTPM429 = &APIError{Status: 429, Message: "Rate limit reached for model `openai/gpt-oss-120b` in organization `org_01abcdefghijklmnop` " +
		"on tokens per minute (TPM): Limit 8000, Used 5344, Requested 6059. Please try again in 6.5s."}
	groqLongTPM429 = &APIError{Status: 429, Message: "Rate limit reached on tokens per minute (TPM). Please try again in 25.5225s."}
	groqTooLarge   = &APIError{Status: 429, Message: "Request too large for model `qwen/qwen3.8-27b` on output tokens per minute (OTPM): Limit 1000, Requested 1557."}
	geminiDaily    = &APIError{Status: 429, Message: "Quota exceeded: GenerateRequestsPerDayPerProjectPerModel-FreeTier. Please retry in 23s."}
	nvidia404      = &APIError{Status: 404, Message: "Function '00000000-1111-2222-3333-444444444444': Not found for account 'AbCdEfGhIjKlMn'"}
)

func TestFailsOverToNextProvider(t *testing.T) {
	groq := &fakeProvider{name: "groq", models: textModels("a"), script: []fakeReply{{err: &APIError{Status: 500, Message: "boom"}}}}
	gemini := &fakeProvider{name: "gemini", models: textModels("g"), script: []fakeReply{{text: "hello"}}}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq", "gemini"}}, groq, gemini)

	res := r.Complete(context.Background(), &Request{Prompt: "p"})

	if !res.OK() || res.Provider != "gemini" || res.Model != "g" || res.Text != "hello" {
		t.Fatalf("unexpected result %+v", res)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "groq(a) 500") {
		t.Errorf("errors = %v", res.Errors)
	}
}

func TestRejectedAnswerMovesOnUntilAValidOne(t *testing.T) {
	// 2026-10-08 的教訓：NVIDIA 回了一段非 JSON 文字，舊版當成功，Gemini 根本沒被試到.
	nvidia := &fakeProvider{name: "nvidia", models: textModels("n1", "n2"), script: []fakeReply{{text: "以下是分析：很重要。"}}}
	gemini := &fakeProvider{name: "gemini", models: textModels("g"), script: []fakeReply{{text: validJSON}}}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"nvidia", "gemini"}}, nvidia, gemini)

	res := r.Complete(context.Background(), &Request{Prompt: "p", Accept: isJSON})

	if res.Provider != "gemini" || res.Text != validJSON {
		t.Fatalf("want gemini answer, got %+v", res)
	}
	if strings.Join(nvidia.calls, ",") != "n1,n2" {
		t.Errorf("nvidia should try every candidate, calls = %v", nvidia.calls)
	}
	if !strings.Contains(strings.Join(res.Errors, "\n"), "回應格式不符") {
		t.Errorf("rejection not recorded: %v", res.Errors)
	}
}

func TestShortRateLimitIsWaitedOutOnTheSameModel(t *testing.T) {
	groq := &fakeProvider{
		name: "groq", models: textModels("gpt-oss-120b", "gpt-oss-20b"),
		script: []fakeReply{{err: groqTPM429}, {text: "ok"}},
	}
	r, clock := newTestRouter(&Settings{TextOrder: []string{"groq"}}, groq)

	res := r.Complete(context.Background(), &Request{Prompt: "p"})

	if res.Model != "gpt-oss-120b" {
		t.Fatalf("should stay on the better model, got %q", res.Model)
	}
	if len(clock.sleeps) != 1 || clock.sleeps[0] != 7500*time.Millisecond {
		t.Errorf("sleeps = %v", clock.sleeps)
	}
	if strings.Join(groq.calls, ",") != "gpt-oss-120b,gpt-oss-120b" {
		t.Errorf("calls = %v", groq.calls)
	}
}

func TestLongOrRepeatedRateLimitSwitchesModel(t *testing.T) {
	groq := &fakeProvider{name: "groq", models: textModels("big", "small"), script: []fakeReply{{err: groqLongTPM429}, {text: "ok"}}}
	r, clock := newTestRouter(&Settings{TextOrder: []string{"groq"}}, groq)
	if res := r.Complete(context.Background(), &Request{Prompt: "p"}); res.Model != "small" {
		t.Errorf("long wait should switch model, got %q", res.Model)
	}
	if len(clock.sleeps) != 0 {
		t.Errorf("should not sleep for a 26s wait, sleeps = %v", clock.sleeps)
	}

	groq = &fakeProvider{name: "groq", models: textModels("big", "small"), script: []fakeReply{{err: groqTPM429}, {err: groqTPM429}, {text: "ok"}}}
	r, clock = newTestRouter(&Settings{TextOrder: []string{"groq"}}, groq)
	if res := r.Complete(context.Background(), &Request{Prompt: "p"}); res.Model != "small" {
		t.Errorf("second rate limit should switch model, got %q", res.Model)
	}
	if len(clock.sleeps) != 1 {
		t.Errorf("should wait only once per model, sleeps = %v", clock.sleeps)
	}
}

func TestUnusableModelsCoolDown(t *testing.T) {
	for name, err := range map[string]error{"too large": groqTooLarge, "daily quota": geminiDaily, "not found": nvidia404} {
		t.Run(name, func(t *testing.T) {
			p := &fakeProvider{name: "groq", models: textModels("bad", "good"), script: []fakeReply{{err: err}, {text: "ok"}, {text: "ok"}}}
			r, clock := newTestRouter(&Settings{TextOrder: []string{"groq"}}, p)
			r.Complete(context.Background(), &Request{Prompt: "p"})
			r.Complete(context.Background(), &Request{Prompt: "p2"})
			if strings.Join(p.calls, ",") != "bad,good,good" {
				t.Errorf("calls = %v", p.calls)
			}
			if len(clock.sleeps) != 0 {
				t.Errorf("must not wait on %s, sleeps = %v", name, clock.sleeps)
			}
		})
	}
}

func TestCooldownExpires(t *testing.T) {
	p := &fakeProvider{name: "groq", models: textModels("bad", "good"), script: []fakeReply{{err: groqTooLarge}, {text: "ok"}, {text: "ok"}}}
	r, clock := newTestRouter(&Settings{TextOrder: []string{"groq"}}, p)
	r.Complete(context.Background(), &Request{Prompt: "p"})
	clock.t = clock.t.Add(tooLargeCooldown + time.Minute)
	r.lists = map[listKey]*candidateList{} // 讓清單重新排序（模擬快取過期）.
	r.Complete(context.Background(), &Request{Prompt: "p2"})
	if p.calls[2] != "bad" {
		t.Errorf("model should be retried after its cooldown, calls = %v", p.calls)
	}
}

func TestProviderBudgetMovesToNextProvider(t *testing.T) {
	slow := &slowProvider{fakeProvider: fakeProvider{name: "nvidia", models: textModels("a", "b", "c"), script: []fakeReply{{err: errors.New("overloaded")}}}}
	gemini := &fakeProvider{name: "gemini", models: textModels("g"), script: []fakeReply{{text: "ok"}}}
	r, clock := newTestRouter(&Settings{TextOrder: []string{"nvidia", "gemini"}, ProviderBudget: 10 * time.Second}, slow, gemini)
	slow.clock = clock

	res := r.Complete(context.Background(), &Request{Prompt: "p"})

	if res.Provider != "gemini" {
		t.Fatalf("got %+v", res)
	}
	if len(slow.calls) != 1 {
		t.Errorf("budget spent after first call, calls = %v", slow.calls)
	}
	if !strings.Contains(strings.Join(res.Errors, "\n"), "時間上限") {
		t.Errorf("errors = %v", res.Errors)
	}
}

// slowProvider 每次呼叫推進 30 秒，並檢查呼叫逾時不超過剩餘預算.
type slowProvider struct {
	fakeProvider
	clock       *fakeClock
	maxDeadline time.Duration
}

func (s *slowProvider) Call(ctx context.Context, model string, req *Request) (string, error) {
	if d, ok := ctx.Deadline(); ok {
		s.maxDeadline = time.Until(d)
	}
	s.clock.t = s.clock.t.Add(30 * time.Second)
	return s.fakeProvider.Call(ctx, model, req)
}

func TestCallDeadlineIsCappedByBudget(t *testing.T) {
	slow := &slowProvider{fakeProvider: fakeProvider{name: "groq", models: textModels("a"), script: []fakeReply{{text: "ok"}}}}
	r := NewRouter(&Settings{TextOrder: []string{"groq"}, ProviderBudget: 5 * time.Second}, slow)
	slow.clock = &fakeClock{t: time.Now()}
	r.Complete(context.Background(), &Request{Prompt: "p"})
	if slow.maxDeadline <= 0 || slow.maxDeadline > 5*time.Second {
		t.Errorf("call deadline %v should be within the 5s budget", slow.maxDeadline)
	}
}

func TestPromoteKeepsOtherCandidates(t *testing.T) {
	p := &fakeProvider{name: "groq", models: textModels("a", "b"), script: []fakeReply{{err: errors.New("503")}, {text: "ok"}, {err: errors.New("503")}, {text: "ok"}}}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq"}}, p)
	if res := r.Complete(context.Background(), &Request{Prompt: "p"}); res.Model != "b" {
		t.Fatalf("got %q", res.Model)
	}
	if res := r.Complete(context.Background(), &Request{Prompt: "p2"}); res.Model != "a" {
		t.Errorf("winner tried first, the other kept as fallback; got %q (calls %v)", res.Model, p.calls)
	}
	if strings.Join(p.calls, ",") != "a,b,b,a" {
		t.Errorf("calls = %v", p.calls)
	}
}

func TestVisionUsesVisionOrderAndSkipsProvidersWithoutVisionModels(t *testing.T) {
	groq := &fakeProvider{name: "groq", models: textModels("text-only"), script: []fakeReply{{text: "should not be called"}}}
	gemini := &fakeProvider{name: "gemini", models: map[Capability][]string{Vision: {"flash"}}, script: []fakeReply{{text: validJSON}}}
	r, _ := newTestRouter(&Settings{VisionOrder: []string{"groq", "gemini"}}, groq, gemini)

	res := r.Complete(context.Background(), &Request{Prompt: "p", Images: []Image{{MIME: "image/jpeg", Data: []byte{1}}}})

	if res.Provider != "gemini" || len(groq.calls) != 0 {
		t.Errorf("res=%+v groq calls=%v", res, groq.calls)
	}
}

func TestErrorsAreRedacted(t *testing.T) {
	p := &fakeProvider{name: "groq", models: textModels("m"), script: []fakeReply{{err: nvidia404}}}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq"}}, p)
	res := r.Complete(context.Background(), &Request{Prompt: "p"})
	joined := strings.Join(res.Errors, "\n")
	for _, leak := range []string{"AbCdEfGhIjKlMn", "00000000-1111"} {
		if strings.Contains(joined, leak) {
			t.Errorf("leaked %q: %s", leak, joined)
		}
	}
	p = &fakeProvider{name: "groq", models: textModels("m"), script: []fakeReply{{err: groqTPM429}, {err: groqTPM429}}}
	r, _ = newTestRouter(&Settings{TextOrder: []string{"groq"}}, p)
	if joined := strings.Join(r.Complete(context.Background(), &Request{Prompt: "p"}).Errors, "\n"); strings.Contains(joined, "org_01abc") {
		t.Errorf("leaked org id: %s", joined)
	}
}

func TestNoProvidersConfigured(t *testing.T) {
	r := NewRouter(&Settings{}, NewGemini("", nil), NewGroq("", nil), NewNVIDIA("", nil))
	res := r.Complete(context.Background(), &Request{Prompt: "p"})
	if res.OK() || len(res.Errors) != 1 {
		t.Errorf("got %+v", res)
	}
	if len(r.Configured()) != 0 {
		t.Errorf("configured = %v", r.Configured())
	}
}

func TestStripsReasoningAndEmptyIsFailure(t *testing.T) {
	p := &fakeProvider{name: "groq", models: textModels("a", "b"), script: []fakeReply{{text: "<think>hmm</think>  "}, {text: "<think>x</think>答案"}}}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq"}}, p)
	res := r.Complete(context.Background(), &Request{Prompt: "p"})
	if res.Text != "答案" || res.Model != "b" {
		t.Errorf("got %+v", res)
	}
}
