package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 決策 b2ea313f 的五個真實 NVIDIA 模型名（文字順序）.
var realNVIDIAModels = []string{
	"nvidia/nemotron-3-ultra-550b-a55b",
	"nvidia/nemotron-3.5-super-vl-120b-a12b",
	"nvidia/nemotron-3-nano-omni-30b-a3b-reasoning",
	"nvidia/nemotron-3.5-lightning-30b-a3b",
	"nvidia/nemotron-3.5-bolt-e4b",
}

// recordedModels 真的 NVIDIA 供應商接 httptest：記下每個 chat completions 請求的 model，
// 並依 status 函式決定回應（404 / 200）.
type recordedModels struct {
	mu     sync.Mutex
	models []string
}

func (r *recordedModels) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.models...)
}

func (r *recordedModels) count(model string) int {
	n := 0
	for _, m := range r.list() {
		if m == model {
			n++
		}
	}
	return n
}

func realNVIDIAServer(t *testing.T, notFound map[string]bool) (Provider, *recordedModels) {
	t.Helper()
	rec := &recordedModels{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(data, &body)
		rec.mu.Lock()
		rec.models = append(rec.models, body.Model)
		rec.mu.Unlock()
		if notFound[body.Model] {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"Function not found for account"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(srv.Close)
	p, ok := NewNVIDIA("k", srv.Client()).(*OpenAICompatible)
	if !ok {
		t.Fatal("NewNVIDIA did not return *OpenAICompatible")
	}
	p.baseURL = srv.URL
	return p, rec
}

// [F33-33] G-02：真實五個名字的看圖判定（規則：skip 字優先，再要看圖標記）.
func TestSupportsVisionRealNVIDIAModels(t *testing.T) {
	check, ok := NewNVIDIA("k", nil).(visionChecker)
	if !ok {
		t.Fatal("NewNVIDIA must implement visionChecker")
	}
	want := []bool{false, true, true, false, false}
	for i, name := range realNVIDIAModels {
		if got := check.SupportsVision(name); got != want[i] {
			t.Errorf("SupportsVision(%q) = %v, want %v", name, got, want[i])
		}
	}
}

// [F33-33] G-02：真實名單走真的 NVIDIA 供應商——看圖只呼叫 super-vl → nano-omni，文字從 ultra 開始.
func TestConfiguredListRealNVIDIAVisionOrder(t *testing.T) {
	settings := func() *Settings {
		return &Settings{
			TextOrder: []string{"nvidia"}, VisionOrder: []string{"nvidia"},
			Models: map[string][]string{"nvidia": realNVIDIAModels},
		}
	}
	ultra, superVL, omni := realNVIDIAModels[0], realNVIDIAModels[1], realNVIDIAModels[2]

	t.Run("vision", func(t *testing.T) {
		p, rec := realNVIDIAServer(t, map[string]bool{superVL: true})
		r, _ := newTestRouter(settings(), p)
		res := r.Complete(context.Background(), visionRequest())
		if !res.OK() || res.Model != omni {
			t.Fatalf("vision must end on nano-omni after super-vl 404: %+v", res)
		}
		if got := strings.Join(rec.list(), ","); got != superVL+","+omni {
			t.Errorf("vision calls = %s, want super-vl then nano-omni only", got)
		}
		for _, skipped := range []string{ultra, realNVIDIAModels[3], realNVIDIAModels[4]} {
			if rec.count(skipped) != 0 {
				t.Errorf("%s cannot see images and must not be called", skipped)
			}
		}
	})

	t.Run("text", func(t *testing.T) {
		p, rec := realNVIDIAServer(t, nil)
		r, _ := newTestRouter(settings(), p)
		res := r.Complete(context.Background(), textReq())
		if !res.OK() || res.Model != ultra {
			t.Fatalf("text must start with ultra-550b: %+v", res)
		}
		if got := rec.list(); len(got) != 1 || got[0] != ultra {
			t.Errorf("text calls = %v, want only ultra", got)
		}
	})
}

// [F33-34] G-03：設定的冷卻秒數要有行為效果（5s：+4s 仍冷卻、+5s 恢復；回滾開關 1s）.
func TestProviderCooldownUsesConfiguredDuration(t *testing.T) {
	cases := []struct {
		name     string
		cooldown time.Duration
		still    time.Duration // 這個時間點仍在冷卻
		over     time.Duration // 這個時間點已恢復
	}{
		{"5s", 5 * time.Second, 4 * time.Second, 5 * time.Second},
		{"1s rollback switch", time.Second, 500 * time.Millisecond, time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nvidia := &funcProvider{name: "nvidia", models: []string{"a"}, fn: failOnce(&APIError{Status: 500})}
			groq := &funcProvider{name: "groq", models: []string{"g"}, fn: answer("ok")}
			r, clock := newTestRouter(&Settings{TextOrder: []string{"nvidia", "groq"}, ProviderCooldown: tc.cooldown}, nvidia, groq)

			r.Complete(context.Background(), textReq())
			start := clock.t

			clock.t = start.Add(tc.still)
			if res := r.Complete(context.Background(), textReq()); res.Provider != "groq" || nvidia.callCount() != 1 {
				t.Fatalf("+%v must still be cooling: %+v nvidia calls=%d", tc.still, res, nvidia.callCount())
			}
			clock.t = start.Add(tc.over)
			if res := r.Complete(context.Background(), textReq()); res.Provider != "nvidia" {
				t.Errorf("+%v the configured cooldown is over, nvidia must be first again: %+v", tc.over, res)
			}
		})
	}
}

// [F33-34] G-12：Settings 比 Claude 自帶的 25s 大時，Claude 仍 ≤25s.
func TestClaudeKeepsLongLimitsWhenSettingsAreLarger(t *testing.T) {
	claude := remainingClaude{newRemainingProvider("claude", "opus")}
	r := NewRouter(&Settings{AttemptTimeout: 60 * time.Second, ProviderBudget: 60 * time.Second}, claude)

	r.Complete(context.Background(), &Request{Prompt: "p", Order: []string{"claude"}})

	if got := claude.remaining[0]; got <= 8*time.Second || got > 25*time.Second {
		t.Errorf("claude call got %v, want within (8s, 25s]; larger Settings must not stretch it", got)
	}
}

// shortLimitClaude 像 Claude 但時限很短，方便測逾時.
type shortLimitClaude struct{ *funcProvider }

func (shortLimitClaude) Limits() (attempt, budget time.Duration) {
	return 30 * time.Millisecond, 50 * time.Millisecond
}

// [F33-34] G-13：Claude 逾時也進供應商冷卻.
func TestClaudeTimeoutCoolsDown(t *testing.T) {
	claude := shortLimitClaude{&funcProvider{name: "claude", models: []string{"opus"}, fn: blockUntilDone}}
	r, _ := newTestRouter(&Settings{}, claude)

	res := r.Complete(context.Background(), &Request{Prompt: "p", Order: []string{"claude"}})

	if res.OK() {
		t.Fatalf("a blocked claude cannot answer: %+v", res)
	}
	if !cooled(r, "claude") {
		t.Error("a claude timeout must cool it down")
	}
}

// [F33-35] G-06：a 逾時（整家冷卻）後 b 成功，冷卻不解除.
func TestSuccessAfterTimeoutKeepsProviderCooled(t *testing.T) {
	nvidia := &funcProvider{name: "nvidia", models: []string{"a", "b"}, fn: func(ctx context.Context, model string, req *Request) (string, error) {
		if model == "a" {
			return blockUntilDone(ctx, model, req)
		}
		return "ok", nil
	}}
	groq := &funcProvider{name: "groq", models: []string{"g"}, fn: answer("ok")}
	r, _ := newTestRouter(&Settings{AttemptTimeout: 30 * time.Millisecond, TextOrder: []string{"nvidia", "groq"}}, nvidia, groq)

	res := r.Complete(context.Background(), textReq())
	if res.Model != "b" || res.Provider != "nvidia" {
		t.Fatalf("b must answer after a timed out: %+v", res)
	}
	if !cooled(r, "nvidia") {
		t.Fatal("the timeout of a must keep nvidia cooled even though b answered")
	}
	before := nvidia.callCount()

	res = r.Complete(context.Background(), textReq())
	if res.Provider != "groq" || nvidia.callCount() != before {
		t.Errorf("the next request must go to groq without touching nvidia: %+v calls %d→%d", res, before, nvidia.callCount())
	}
}

// [F33-35] G-07：列模型途中呼叫端期限到，不冷卻.
func TestCallerDeadlineDuringListingDoesNotCool(t *testing.T) {
	nvidia := &funcProvider{name: "nvidia", fn: answer("never"), listFn: func(ctx context.Context) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	groq := &funcProvider{name: "groq", models: []string{"g"}, fn: answer("ok")}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"nvidia", "groq"}}, nvidia, groq)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	res := r.Complete(ctx, textReq())

	if cooled(r, "nvidia") {
		t.Error("the caller's deadline during listing must not cool the provider")
	}
	if !strings.Contains(strings.Join(res.Errors, "\n"), "nvidia(list)") {
		t.Errorf("the listing failure must be recorded: %v", res.Errors)
	}
}

// [F33-35] G-08：真時鐘下，呼叫端期限與單次期限幾乎同時到，不得誤記冷卻.
// 每輪建全新 Router（否則前一輪誤記的冷卻會蓋住後面的觀測）；正對照證明這個 harness 看得到真時鐘 Router 的冷卻.
func TestCallerDeadlineRaceDoesNotCoolRealClock(t *testing.T) {
	const rounds = 25

	t.Run("positive control: a provider 503 is visible as cooled", func(t *testing.T) {
		for i := range rounds {
			nvidia := &funcProvider{name: "nvidia", models: []string{"a"}, fn: failWith(&APIError{Status: 503})}
			r := NewRouter(&Settings{TextOrder: []string{"nvidia"}}, nvidia)
			r.Complete(context.Background(), textReq())
			if !cooled(r, "nvidia") {
				t.Fatalf("round %d: the harness cannot see a real-clock cooldown, 0 mis-cools would mean nothing", i)
			}
		}
	})

	misCooled := 0
	for range rounds {
		nvidia := &funcProvider{name: "nvidia", models: []string{"a"}, fn: blockUntilDone}
		r := NewRouter(&Settings{TextOrder: []string{"nvidia"}}, nvidia)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		r.Complete(ctx, textReq())
		cancel()
		if cooled(r, "nvidia") {
			misCooled++
		}
	}
	t.Logf("misCooled=%d/%d", misCooled, rounds)
	if misCooled > 0 {
		t.Errorf("the caller's deadline was recorded as a provider failure %d/%d times", misCooled, rounds)
	}
}

// [F33-36] G-09：清單有 5 個，m1 被 404 冷卻後，第二個請求輪到第 5 個.
func TestConfiguredListFifthModelReachableAfterEarlierDead(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	nvidia := &funcProvider{name: "nvidia", fn: func(_ context.Context, model string, _ *Request) (string, error) {
		mu.Lock()
		calls = append(calls, model)
		mu.Unlock()
		if model == "m1" || model == "m2" {
			return "", nvidia404
		}
		return "", errors.New("x")
	}}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"nvidia"}, Models: map[string][]string{"nvidia": {"m1", "m2", "m3", "m4", "m5"}}}, nvidia)

	r.Complete(context.Background(), textReq())
	r.Complete(context.Background(), textReq())

	// [F36-02] 每請求 3 個模型：第一次 m1、m2（404 冷卻）、m3；第二次跳過 m1、m2，試 m3、m4、m5.
	want := "m1,m2,m3,m3,m4,m5"
	if got := strings.Join(calls, ","); got != want {
		t.Errorf("calls = %s, want %s (the 5th model is reachable once m1 and m2 are dead)", got, want)
	}
}

// allVisionFake 任何模型都算能看圖的假供應商（用來單獨驗證名字原樣傳遞）.
type allVisionFake struct{ fakeProvider }

func (*allVisionFake) SupportsVision(string) bool { return true }

// [F33-36] G-10：清單模式的 model id 大小寫原樣傳給供應商（文字與看圖）.
func TestConfiguredListKeepsModelIDCase(t *testing.T) {
	const name = "Groq/Mixed-Case"
	models := map[string][]string{"groq": {name}}

	text := &fakeProvider{name: "groq", models: textModels("auto"), script: okScript()}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq"}, Models: models}, text)
	r.Complete(context.Background(), &Request{Prompt: "p"})
	if len(text.calls) == 0 || text.calls[0] != name {
		t.Errorf("text calls = %v, want first %q unchanged", text.calls, name)
	}

	vision := &allVisionFake{fakeProvider{name: "groq", models: textModels("auto"), script: okScript()}}
	r, _ = newTestRouter(&Settings{VisionOrder: []string{"groq"}, Models: models}, vision)
	r.Complete(context.Background(), visionRequest())
	if len(vision.calls) == 0 || vision.calls[0] != name {
		t.Errorf("vision calls = %v, want first %q unchanged", vision.calls, name)
	}
}

// [F33-37] D-03：Gemini 列模型失敗退回別名、不冷卻；同一個 CandidateTTL 內不再列.
func TestGeminiListingFailureFallsBackWithoutCooldown(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`)
	}))
	t.Cleanup(srv.Close)
	g, ok := NewGemini("k", srv.Client()).(*Gemini)
	if !ok {
		t.Fatal("NewGemini did not return *Gemini")
	}
	g.baseURL = srv.URL
	r, _ := newTestRouter(&Settings{TextOrder: []string{"gemini"}}, g)

	res := r.Complete(context.Background(), textReq())
	if !res.OK() || res.Model != geminiFallbackAlias {
		t.Fatalf("a failed listing must fall back to the alias: %+v", res)
	}
	if cooled(r, "gemini") {
		t.Error("a failed Gemini listing must not cool the provider (decision D-03)")
	}
	r.Complete(context.Background(), textReq())

	mu.Lock()
	defer mu.Unlock()
	listings, calls := 0, 0
	for _, p := range paths {
		if strings.HasSuffix(p, "/models") {
			listings++
		}
		if strings.HasSuffix(p, ":generateContent") {
			calls++
			if !strings.HasSuffix(p, "/models/"+geminiFallbackAlias+":generateContent") {
				t.Errorf("call path %s must use the alias", p)
			}
		}
	}
	if listings != 1 || calls != 2 {
		t.Errorf("listings = %d (want 1 within CandidateTTL), calls = %d (want 2); paths %s", listings, calls, fmt.Sprint(paths))
	}
}
