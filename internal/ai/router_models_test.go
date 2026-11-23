package ai

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// visionFake 依名稱規則回答 SupportsVision 的假供應商（像 NVIDIA 的 "vl" 規則）.
type visionFake struct{ fakeProvider }

func (v *visionFake) SupportsVision(model string) bool { return strings.Contains(model, "vl") }

func okScript() []fakeReply { return []fakeReply{{text: "ok"}} }

func failScript() []fakeReply { return []fakeReply{{err: errors.New("x")}} }

func joinCalls(f *fakeProvider) string { return strings.Join(f.calls, ",") }

func visionRequest() *Request {
	return &Request{Prompt: "p", Images: []Image{{MIME: "image/jpeg", Data: []byte{1}}}}
}

// [F33-01] 清單順序即嘗試順序，不被自動排名取代.
func TestConfiguredListOrderIsObeyed(t *testing.T) {
	groq := &fakeProvider{name: "groq", models: textModels("a", "b", "c", "d"), script: okScript()}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq"}, Models: map[string][]string{"groq": {"c", "a", "b"}}}, groq)

	res := r.Complete(context.Background(), &Request{Prompt: "p"})

	if !res.OK() || len(groq.calls) == 0 || groq.calls[0] != "c" {
		t.Fatalf("first call must be the first listed model c, calls = %v res = %+v", groq.calls, res)
	}
}

// [F33-01] 清單內全失敗時，自動排名專有的模型不得被呼叫.
func TestConfiguredListNeverCallsModelsOutsideList(t *testing.T) {
	groq := &fakeProvider{name: "groq", models: textModels("a", "b", "c", "d"), script: []fakeReply{{err: nvidia404}}}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq"}, Models: map[string][]string{"groq": {"a", "b"}}}, groq)

	res := r.Complete(context.Background(), &Request{Prompt: "p"})

	if res.OK() {
		t.Fatalf("every listed model failed, result must not be OK: %+v", res)
	}
	if joinCalls(groq) != "a,b" {
		t.Errorf("only listed models may be called, calls = %v", groq.calls)
	}

	groq = &fakeProvider{name: "groq", models: textModels("a", "b", "c", "d"), script: []fakeReply{{err: nvidia404}}}
	gemini := &fakeProvider{name: "gemini", models: textModels("g"), script: okScript()}
	r, _ = newTestRouter(&Settings{TextOrder: []string{"groq", "gemini"}, Models: map[string][]string{"groq": {"a", "b"}}}, groq, gemini)
	if res := r.Complete(context.Background(), &Request{Prompt: "p"}); res.Provider != "gemini" {
		t.Errorf("should move on to the next provider, got %+v", res)
	}
}

// [F33-01] 清單模式零網路：不呼叫 Candidates.
func TestConfiguredListSkipsModelListing(t *testing.T) {
	groq := &fakeProvider{name: "groq", models: textModels("a", "b"), script: okScript()}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq"}, Models: map[string][]string{"groq": {"x"}}}, groq)

	r.Complete(context.Background(), &Request{Prompt: "p"})

	if groq.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0", groq.listCalls)
	}
}

// [F33-01] nil / 空切片 / 缺 key = 自動排名；同請求設了清單就只走清單.
func TestEmptyOrMissingListUsesAutoRanking(t *testing.T) {
	cases := map[string]map[string][]string{
		"nil map":     nil,
		"empty slice": {"groq": {}},
		"nil slice":   {"groq": nil},
		"missing key": {"nvidia": {"n"}},
		"claude key":  {"claude": {"x"}},
		"empty map":   {},
	}
	for name, models := range cases {
		t.Run(name, func(t *testing.T) {
			groq := &fakeProvider{name: "groq", models: textModels("a", "b"), script: okScript()}
			r, _ := newTestRouter(&Settings{TextOrder: []string{"groq"}, Models: models}, groq)
			r.Complete(context.Background(), &Request{Prompt: "p"})
			if groq.listCalls != 1 || joinCalls(groq) != "a" {
				t.Errorf("auto ranking expected: listCalls=%d calls=%v", groq.listCalls, groq.calls)
			}
		})
	}

	groq := &fakeProvider{name: "groq", models: textModels("a", "b"), script: okScript()}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq"}, Models: map[string][]string{"groq": {"x"}}}, groq)
	r.Complete(context.Background(), &Request{Prompt: "p"})
	if groq.listCalls != 0 || joinCalls(groq) != "x" {
		t.Errorf("list mode expected: listCalls=%d calls=%v", groq.listCalls, groq.calls)
	}
}

// [F33-01] 清單內模型 404 進模型層冷卻，下一請求跳過它；冷卻期滿再試.
func TestConfiguredListNotFoundCoolsModelThenNext(t *testing.T) {
	groq := &fakeProvider{name: "groq", models: textModels("auto"), script: []fakeReply{{err: nvidia404}, {text: "ok"}}}
	r, clock := newTestRouter(&Settings{TextOrder: []string{"groq"}, Models: map[string][]string{"groq": {"a", "b"}}}, groq)

	r.Complete(context.Background(), &Request{Prompt: "p"})
	r.Complete(context.Background(), &Request{Prompt: "p2"})
	if joinCalls(groq) != "a,b,b" {
		t.Fatalf("calls = %v, want a,b,b", groq.calls)
	}

	clock.t = clock.t.Add(notFoundCooldown + time.Minute)
	r.Complete(context.Background(), &Request{Prompt: "p3"})
	if joinCalls(groq) != "a,b,b,a" {
		t.Errorf("model must be retried once its cooldown is over, calls = %v", groq.calls)
	}
}

// [F33-01] 清單全 404 時換下一家，Errors 留有每個失敗模型.
func TestConfiguredListExhaustedMovesToNextProvider(t *testing.T) {
	nvidia := &fakeProvider{name: "nvidia", models: textModels("auto"), script: []fakeReply{{err: nvidia404}}}
	gemini := &fakeProvider{name: "gemini", models: textModels("g"), script: okScript()}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"nvidia", "gemini"}, Models: map[string][]string{"nvidia": {"a", "b"}}}, nvidia, gemini)

	res := r.Complete(context.Background(), &Request{Prompt: "p"})

	if res.Provider != "gemini" {
		t.Fatalf("got %+v", res)
	}
	joined := strings.Join(res.Errors, "\n")
	for _, want := range []string{"nvidia(a)", "nvidia(b)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("errors missing %s: %v", want, res.Errors)
		}
	}
}

// [F33-01] 清單比 MaxAttempts 長：單請求只試前 4 個.
func TestConfiguredListRespectsMaxAttempts(t *testing.T) {
	groq := &fakeProvider{name: "groq", models: textModels("auto"), script: failScript()}
	list := []string{"m1", "m2", "m3", "m4", "m5", "m6"}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq"}, Models: map[string][]string{"groq": list}}, groq)

	r.Complete(context.Background(), &Request{Prompt: "p"})

	// [F36-02] 每請求跨供應商合計 3 個模型，比每家 MaxAttempts(4) 更先到.
	if joinCalls(groq) != "m1,m2,m3" {
		t.Errorf("calls = %v, want exactly the first MaxTotalAttempts (3) models", groq.calls)
	}
}

// [F33-01] 看圖請求先過 SupportsVision.
func TestConfiguredListVisionSkipsNonVisionModels(t *testing.T) {
	nvidia := &visionFake{fakeProvider{name: "nvidia", models: textModels("auto"), script: okScript()}}
	list := []string{"nvidia/x-ultra-550b", "nvidia/y-vl-120b"}
	r, _ := newTestRouter(&Settings{VisionOrder: []string{"nvidia"}, Models: map[string][]string{"nvidia": list}}, nvidia)

	res := r.Complete(context.Background(), visionRequest())

	if !res.OK() || joinCalls(&nvidia.fakeProvider) != "nvidia/y-vl-120b" {
		t.Errorf("only the vision-capable model may be called, calls = %v", nvidia.calls)
	}
}

// [F33-01] 同一份清單的文字請求不過濾.
func TestSameListServesTextUnfiltered(t *testing.T) {
	nvidia := &visionFake{fakeProvider{name: "nvidia", models: textModels("auto"), script: okScript()}}
	list := []string{"nvidia/x-ultra-550b", "nvidia/y-vl-120b"}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"nvidia"}, Models: map[string][]string{"nvidia": list}}, nvidia)

	r.Complete(context.Background(), &Request{Prompt: "p"})

	if len(nvidia.calls) == 0 || nvidia.calls[0] != "nvidia/x-ultra-550b" {
		t.Errorf("text requests are not filtered, calls = %v", nvidia.calls)
	}
}

// [F33-01] 看圖過濾後為空：這家零呼叫、不退回自動排名、換下一家.
func TestConfiguredListNoVisionModelMovesOn(t *testing.T) {
	nvidia := &visionFake{fakeProvider{name: "nvidia", models: map[Capability][]string{Vision: {"auto-vl"}}, script: okScript()}}
	gemini := &fakeProvider{name: "gemini", models: map[Capability][]string{Vision: {"flash"}}, script: okScript()}
	list := []string{"nvidia/x-ultra-550b", "nvidia/z-big-70b"}
	r, _ := newTestRouter(&Settings{VisionOrder: []string{"nvidia", "gemini"}, Models: map[string][]string{"nvidia": list}}, nvidia, gemini)

	res := r.Complete(context.Background(), visionRequest())

	if res.Provider != "gemini" || len(nvidia.calls) != 0 {
		t.Errorf("res = %+v nvidia calls = %v", res, nvidia.calls)
	}
	if nvidia.listCalls != 0 {
		t.Errorf("must not fall back to auto ranking, listCalls = %d", nvidia.listCalls)
	}
}

// [F33-01] 清單模式不 promote：成功的第 2 個模型不會讓下一請求改從它開始.
func TestConfiguredListIsNotPromoted(t *testing.T) {
	groq := &fakeProvider{name: "groq", models: textModels("auto"), script: []fakeReply{{err: errors.New("x")}, {text: "ok"}}}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq"}, Models: map[string][]string{"groq": {"a", "b"}}}, groq)

	r.Complete(context.Background(), &Request{Prompt: "p"})
	r.Complete(context.Background(), &Request{Prompt: "p2"})

	if joinCalls(groq) != "a,b,a" {
		t.Errorf("operator order must win, calls = %v want a,b,a", groq.calls)
	}
}

// [F33-01] Settings.Models["claude"] 被忽略，Claude 仍用自己的單一模型.
func TestModelsForClaudeAreIgnored(t *testing.T) {
	claude := &fakeProvider{name: "claude", models: textModels("the-model"), script: okScript()}
	r, _ := newTestRouter(&Settings{Models: map[string][]string{"claude": {"x"}}}, claude)

	res := r.Complete(context.Background(), &Request{Prompt: "p", Order: []string{"claude"}})

	if !res.OK() || joinCalls(claude) != "the-model" {
		t.Errorf("claude must keep its own model, calls = %v", claude.calls)
	}
	if _, ok := r.settings.Models["claude"]; ok {
		t.Errorf("claude key should not survive NewRouter: %v", r.settings.Models)
	}
}

// [F33-01] NewRouter 深拷貝清單：呼叫端事後改動不影響路由；同清單重複項去重.
func TestModelsAreDeepClonedAndDeduped(t *testing.T) {
	list := []string{"a", "a", "b"}
	models := map[string][]string{"groq": list}
	groq := &fakeProvider{name: "groq", models: textModels("auto"), script: failScript()}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq"}, Models: models}, groq)

	list[0] = "mutated"
	models["groq"] = []string{"other"}
	r.Complete(context.Background(), &Request{Prompt: "p"})

	if joinCalls(groq) != "a,b" {
		t.Errorf("calls = %v, want a,b (cloned, deduped)", groq.calls)
	}
}
