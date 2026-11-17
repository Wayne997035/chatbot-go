package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// funcProvider 行為由函式決定的假供應商，自帶鎖（可被多個 goroutine 同時呼叫）.
type funcProvider struct {
	name   string
	models []string
	fn     func(ctx context.Context, model string, req *Request) (string, error)
	listFn func(ctx context.Context) ([]string, error)

	mu        sync.Mutex
	calls     []string
	listCalls int
}

func (f *funcProvider) Name() string { return f.name }

func (f *funcProvider) Candidates(ctx context.Context, _ Capability) ([]string, error) {
	f.mu.Lock()
	f.listCalls++
	f.mu.Unlock()
	if f.listFn != nil {
		return f.listFn(ctx)
	}
	return f.models, nil
}

func (f *funcProvider) Call(ctx context.Context, model string, req *Request) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, model)
	f.mu.Unlock()
	return f.fn(ctx, model, req)
}

func (f *funcProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func failWith(err error) func(context.Context, string, *Request) (string, error) {
	return func(context.Context, string, *Request) (string, error) { return "", err }
}

func answer(text string) func(context.Context, string, *Request) (string, error) {
	return func(context.Context, string, *Request) (string, error) { return text, nil }
}

// blockUntilDone 像卡住的供應商：一直等到 ctx 結束.
func blockUntilDone(ctx context.Context, _ string, _ *Request) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// failOnce 第一次呼叫失敗，之後成功.
func failOnce(err error) func(context.Context, string, *Request) (string, error) {
	var mu sync.Mutex
	count := 0
	return func(context.Context, string, *Request) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		count++
		if count == 1 {
			return "", err
		}
		return "ok", nil
	}
}

// countProvider 包住真的供應商並計算 Call 次數.
type countProvider struct {
	Provider
	mu sync.Mutex
	n  int
}

func (c *countProvider) Call(ctx context.Context, model string, req *Request) (string, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.Provider.Call(ctx, model, req)
}

func (c *countProvider) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// limitedProvider 像 Claude：自帶單次與總時限.
type limitedProvider struct{ *funcProvider }

func (limitedProvider) Limits() (attempt, budget time.Duration) {
	return 25 * time.Second, 25 * time.Second
}

func textReq() *Request { return &Request{Prompt: "p"} }

// cooled 這家供應商的文字能力是否在冷卻中.
func cooled(r *Router, provider string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	until, ok := r.down[providerKey{provider, Text}]
	return ok && r.now().Before(until)
}

// closedServerProvider 回傳一個真的 NVIDIA 供應商，指向已關閉的 server（connection refused）.
func closedServerProvider(t *testing.T) Provider {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	client, url := srv.Client(), srv.URL
	srv.Close()
	p, ok := NewNVIDIA("k", client).(*OpenAICompatible)
	if !ok {
		t.Fatal("NewNVIDIA did not return *OpenAICompatible")
	}
	p.baseURL = url
	return p
}

// [F33-04] 逾時 / 連不上 / 5xx 讓供應商進冷卻：下一個請求不先試它.
func TestProviderCooldownTriggers(t *testing.T) {
	cases := []struct {
		name     string
		settings Settings
		build    func(t *testing.T) Provider
	}{
		{"attempt timeout", Settings{AttemptTimeout: 30 * time.Millisecond}, func(*testing.T) Provider {
			return &funcProvider{name: "nvidia", models: []string{"a"}, fn: blockUntilDone}
		}},
		{"connection refused", Settings{Models: map[string][]string{"nvidia": {"m"}}}, closedServerProvider},
		{"500", Settings{}, func(*testing.T) Provider {
			return &funcProvider{name: "nvidia", models: []string{"a"}, fn: failWith(&APIError{Status: 500})}
		}},
		{"502", Settings{}, func(*testing.T) Provider {
			return &funcProvider{name: "nvidia", models: []string{"a"}, fn: failWith(&APIError{Status: 502})}
		}},
		{"503", Settings{}, func(*testing.T) Provider {
			return &funcProvider{name: "nvidia", models: []string{"a"}, fn: failWith(&APIError{Status: 503})}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nvidia := &countProvider{Provider: tc.build(t)}
			groq := &funcProvider{name: "groq", models: []string{"g"}, fn: answer("ok")}
			tc.settings.TextOrder = []string{"nvidia", "groq"}
			r, _ := newTestRouter(&tc.settings, nvidia, groq)

			if res := r.Complete(context.Background(), textReq()); res.Provider != "groq" {
				t.Fatalf("first request should fall through to groq: %+v", res)
			}
			if nvidia.count() != 1 {
				t.Fatalf("nvidia should have been tried once, got %d", nvidia.count())
			}
			if res := r.Complete(context.Background(), textReq()); res.Provider != "groq" {
				t.Fatalf("second request: %+v", res)
			}
			if nvidia.count() != 1 {
				t.Errorf("a cooled provider must not be tried first, nvidia calls = %d", nvidia.count())
			}
		})
	}
}

// [F33-04] 不是供應商故障的失敗不冷卻整家.
func TestNonProviderFailuresDoNotCoolProvider(t *testing.T) {
	cases := []struct {
		name string
		fn   func(context.Context, string, *Request) (string, error)
		// dead 模型本身被冷卻（404 等），所以下一個請求不會再呼叫它.
		dead bool
	}{
		{"429", failWith(groqTPM429), false},
		{"404", failWith(nvidia404), true},
		{"daily quota", failWith(geminiDaily), true},
		{"request too large", failWith(groqTooLarge), true},
		{"400", failWith(&APIError{Status: 400, Message: "bad"}), false},
		{"403", failWith(&APIError{Status: 403, Message: "forbidden"}), false},
		{"empty", answer(""), false},
		{"rejected", answer("bad"), false},
		{"plain error", failWith(errors.New("overloaded")), false},
	}
	accept := func(s string) bool { return s != "bad" }
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nvidia := &funcProvider{name: "nvidia", models: []string{"a"}, fn: tc.fn}
			groq := &funcProvider{name: "groq", models: []string{"g"}, fn: answer("ok")}
			r, _ := newTestRouter(&Settings{TextOrder: []string{"nvidia", "groq"}}, nvidia, groq)

			r.Complete(context.Background(), &Request{Prompt: "p", Accept: accept})
			r.Complete(context.Background(), &Request{Prompt: "p", Accept: accept})

			if cooled(r, "nvidia") {
				t.Errorf("%s must not cool the provider", tc.name)
			}
			if got := r.coolestLast([]string{"nvidia", "groq"}, Text); strings.Join(got, ",") != "nvidia,groq" {
				t.Errorf("order = %v, nvidia must stay first", got)
			}
			want := 2
			if tc.dead {
				want = 1
			}
			if nvidia.callCount() != want {
				t.Errorf("nvidia calls = %d, want %d", nvidia.callCount(), want)
			}
		})
	}
}

// [F33-04] 404 只冷卻模型，同供應商下一個請求仍先試（清單內另一個模型）.
func TestNotFoundDoesNotCoolProvider(t *testing.T) {
	groq := &funcProvider{name: "groq", models: []string{"a", "b"}, fn: func(_ context.Context, model string, _ *Request) (string, error) {
		if model == "a" {
			return "", nvidia404
		}
		return "ok", nil
	}}
	gemini := &funcProvider{name: "gemini", models: []string{"g"}, fn: answer("ok")}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq", "gemini"}}, groq, gemini)

	r.Complete(context.Background(), textReq())
	res := r.Complete(context.Background(), textReq())

	if res.Provider != "groq" || gemini.callCount() != 0 {
		t.Errorf("groq must still be first after a model-level 404: %+v gemini calls=%d", res, gemini.callCount())
	}
	if cooled(r, "groq") {
		t.Error("404 must not cool the provider")
	}
}

// [F33-04] 冷卻的鍵含能力：文字逾時不牽連看圖，反之亦然.
func TestProviderCooldownIsPerCapability(t *testing.T) {
	for _, failVision := range []bool{false, true} {
		nvidia := &funcProvider{name: "nvidia", models: []string{"a"}, fn: func(_ context.Context, _ string, req *Request) (string, error) {
			if (len(req.Images) > 0) == failVision {
				return "", &APIError{Status: 503}
			}
			return "ok", nil
		}}
		groq := &funcProvider{name: "groq", models: []string{"g"}, fn: answer("ok")}
		order := []string{"nvidia", "groq"}
		r, _ := newTestRouter(&Settings{TextOrder: order, VisionOrder: order}, nvidia, groq)
		failing, other := textReq(), visionRequest()
		if failVision {
			failing, other = other, failing
		}

		r.Complete(context.Background(), failing) // 故障那種能力進冷卻.
		before := nvidia.callCount()
		if res := r.Complete(context.Background(), other); res.Provider != "nvidia" {
			t.Errorf("failVision=%v: the other capability must still try nvidia first: %+v", failVision, res)
		}
		if nvidia.callCount() != before+1 {
			t.Errorf("failVision=%v: nvidia calls = %d, want %d", failVision, nvidia.callCount(), before+1)
		}
		if res := r.Complete(context.Background(), failing); res.Provider != "groq" {
			t.Errorf("failVision=%v: the failing capability must demote nvidia: %+v", failVision, res)
		}
	}
}

// [F33-04] 冷卻 60 秒：+59 秒仍冷卻，+60 秒（now == until）已到期要再試.
func TestProviderCooldownExpiresAndRetries(t *testing.T) {
	nvidia := &funcProvider{name: "nvidia", models: []string{"a"}, fn: failOnce(&APIError{Status: 500})}
	groq := &funcProvider{name: "groq", models: []string{"g"}, fn: answer("ok")}
	r, clock := newTestRouter(&Settings{TextOrder: []string{"nvidia", "groq"}}, nvidia, groq)

	r.Complete(context.Background(), textReq())
	start := clock.t

	clock.t = start.Add(59 * time.Second)
	if res := r.Complete(context.Background(), textReq()); res.Provider != "groq" || nvidia.callCount() != 1 {
		t.Fatalf("+59s must still be cooling: %+v nvidia calls=%d", res, nvidia.callCount())
	}

	clock.t = start.Add(defaultProviderCooldown)
	res := r.Complete(context.Background(), textReq())
	if res.Provider != "nvidia" || nvidia.callCount() != 2 {
		t.Errorf("+60s the cooldown is over and nvidia must go first again: %+v nvidia calls=%d", res, nvidia.callCount())
	}
}

// [F33-04] 三家都在冷卻：仍依原順序嘗試並可成功.
func TestAllProvidersCooledStillTried(t *testing.T) {
	down := &APIError{Status: 500}
	groq := &funcProvider{name: "groq", models: []string{"a"}, fn: failWith(down)}
	gemini := &funcProvider{name: "gemini", models: []string{"b"}, fn: failOnce(down)}
	nvidia := &funcProvider{name: "nvidia", models: []string{"c"}, fn: failOnce(down)}
	r, _ := newTestRouter(&Settings{}, groq, gemini, nvidia)

	if res := r.Complete(context.Background(), textReq()); res.OK() {
		t.Fatalf("every provider failed on the first request: %+v", res)
	}
	for _, name := range []string{"groq", "gemini", "nvidia"} {
		if !cooled(r, name) {
			t.Fatalf("%s should be cooled", name)
		}
	}

	res := r.Complete(context.Background(), textReq())
	if res.Provider != "gemini" {
		t.Errorf("all cooled still tries in the original order (groq fails, gemini answers): %+v", res)
	}
	if groq.callCount() != 2 {
		t.Errorf("groq was first in the original order, calls = %d", groq.callCount())
	}
}

// [F33-04] 未冷卻的永遠先於冷卻的；有未冷卻的成功時冷卻的不被呼叫.
func TestCooledProviderIsTriedLast(t *testing.T) {
	nvidia := &funcProvider{name: "nvidia", models: []string{"a"}, fn: failOnce(&APIError{Status: 502})}
	groq := &funcProvider{name: "groq", models: []string{"g"}, fn: answer("ok")}
	gemini := &funcProvider{name: "gemini", models: []string{"m"}, fn: answer("ok")}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"nvidia", "groq", "gemini"}}, nvidia, groq, gemini)

	r.Complete(context.Background(), textReq())
	if got := strings.Join(r.coolestLast([]string{"nvidia", "groq", "gemini"}, Text), ","); got != "groq,gemini,nvidia" {
		t.Errorf("order = %s, want groq,gemini,nvidia", got)
	}
	res := r.Complete(context.Background(), textReq())
	if res.Provider != "groq" || nvidia.callCount() != 1 {
		t.Errorf("res = %+v nvidia calls = %d, want it untouched", res, nvidia.callCount())
	}
}

// [F33-04] 呼叫端期限到或取消不是供應商的錯，不記冷卻.
func TestCallerCancelDoesNotCoolProvider(t *testing.T) {
	cases := map[string]func() (context.Context, context.CancelFunc){
		"caller deadline": func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 30*time.Millisecond)
		},
		"caller cancel": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(30*time.Millisecond, cancel)
			return ctx, cancel
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			nvidia := &funcProvider{name: "nvidia", models: []string{"a"}, fn: blockUntilDone}
			groq := &funcProvider{name: "groq", models: []string{"g"}, fn: answer("ok")}
			r, _ := newTestRouter(&Settings{TextOrder: []string{"nvidia", "groq"}}, nvidia, groq)
			ctx, cancel := mk()
			defer cancel()

			res := r.Complete(ctx, textReq())

			if res.OK() || len(res.Errors) == 0 {
				t.Errorf("the failure must be reported: %+v", res)
			}
			if cooled(r, "nvidia") {
				t.Error("the caller's cancellation must not cool the provider")
			}
			if got := r.coolestLast([]string{"nvidia", "groq"}, Text); got[0] != "nvidia" {
				t.Errorf("nvidia must still be first, order = %v", got)
			}
		})
	}
}

// [F33-03] 列模型也受單次上限約束；逾時視為供應商故障.
func TestListingIsBoundedAndCoolsProvider(t *testing.T) {
	var remaining time.Duration
	nvidia := &funcProvider{name: "nvidia", listFn: func(ctx context.Context) ([]string, error) {
		d, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("listing without a deadline")
		}
		remaining = time.Until(d)
		<-ctx.Done()
		return nil, ctx.Err()
	}, fn: answer("never")}
	groq := &funcProvider{name: "groq", models: []string{"g"}, fn: answer("ok")}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"nvidia", "groq"}, AttemptTimeout: 50 * time.Millisecond}, nvidia, groq)

	res := r.Complete(context.Background(), textReq())

	if res.Provider != "groq" {
		t.Fatalf("listing hang must not block the request: %+v", res)
	}
	if remaining <= 0 || remaining > 50*time.Millisecond {
		t.Errorf("listing deadline = %v, want within (0, 50ms]", remaining)
	}
	if !cooled(r, "nvidia") {
		t.Error("a listing timeout must cool the provider")
	}
	if !strings.Contains(strings.Join(res.Errors, "\n"), "nvidia(list)") {
		t.Errorf("listing failure must be recorded: %v", res.Errors)
	}
}

// [F33-07] Claude 逾時 / 5xx 仍進供應商冷卻.
func TestClaudeStillCoolsDownOn5xx(t *testing.T) {
	claude := limitedProvider{&funcProvider{name: "claude", models: []string{"opus"}, fn: failOnce(&APIError{Status: 500})}}
	groq := &funcProvider{name: "groq", models: []string{"g"}, fn: answer("ok")}
	r, _ := newTestRouter(&Settings{}, claude, groq)
	req := func() *Request { return &Request{Prompt: "p", Order: []string{"claude", "groq"}} }

	r.Complete(context.Background(), req())
	res := r.Complete(context.Background(), req())

	if !cooled(r, "claude") {
		t.Error("claude 5xx must cool it down")
	}
	if res.Provider != "groq" || claude.callCount() != 1 {
		t.Errorf("claude must not be tried first again: %+v calls=%d", res, claude.callCount())
	}
}

// [F33-04] 同請求 Order=[claude,groq]：claude 5xx 後排最後；404 / 429 不冷卻.
func TestClaudeCooldownDemotesToLast(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantDown bool
	}{
		{"500", &APIError{Status: 500}, true},
		{"404", nvidia404, false},
		{"429", groqTPM429, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claude := limitedProvider{&funcProvider{name: "claude", models: []string{"opus"}, fn: failWith(tc.err)}}
			groq := &funcProvider{name: "groq", models: []string{"g"}, fn: answer("ok")}
			r, _ := newTestRouter(&Settings{}, claude, groq)
			order := []string{"claude", "groq"}

			r.Complete(context.Background(), &Request{Prompt: "p", Order: order})

			got := strings.Join(r.coolestLast(order, Text), ",")
			if tc.wantDown && got != "groq,claude" {
				t.Errorf("order = %s, want groq,claude", got)
			}
			if !tc.wantDown && got != "claude,groq" {
				t.Errorf("order = %s, claude must stay first for %s", got, tc.name)
			}
		})
	}
}

// [F33-05] 預設限流不等：不 sleep，直接換下一個模型.
func TestRateLimitZeroWaitSwitchesWithoutSleep(t *testing.T) {
	groq := &fakeProvider{name: "groq", models: textModels("m1", "m2"), script: []fakeReply{{err: groqTPM429}, {text: "ok"}}}
	r, clock := newTestRouter(&Settings{TextOrder: []string{"groq"}}, groq)

	res := r.Complete(context.Background(), textReq())

	if len(clock.sleeps) != 0 {
		t.Errorf("sleeps = %v, want none", clock.sleeps)
	}
	if joinCalls(groq) != "m1,m2" || res.Model != "m2" {
		t.Errorf("calls = %v res = %+v", groq.calls, res)
	}
	if cooled(r, "groq") {
		t.Error("a 429 must not cool the provider")
	}
}

// [F33-04] 並行請求同時寫入冷卻：以鎖序列化，不 race（跑 go test -race）.
func TestConcurrentCompleteIsRaceFree(t *testing.T) {
	nvidia := &funcProvider{name: "nvidia", models: []string{"a"}, fn: failWith(&APIError{Status: 503})}
	groq := &funcProvider{name: "groq", models: []string{"g"}, fn: answer("ok")}
	r := NewRouter(&Settings{TextOrder: []string{"nvidia", "groq"}, ProviderCooldown: time.Millisecond}, nvidia, groq)

	const workers, rounds = 16, 25
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range rounds {
				if res := r.Complete(context.Background(), textReq()); !res.OK() {
					t.Errorf("groq should always answer: %+v", res)
					return
				}
				time.Sleep(time.Millisecond) // 讓冷卻到期，nvidia 一再被重試、一再寫入冷卻.
			}
		}()
	}
	close(start)
	wg.Wait()

	if nvidia.callCount() == 0 {
		t.Error("nvidia was never tried, the cooldown write path was not exercised")
	}
}
