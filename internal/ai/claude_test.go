package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

type claudeCapture struct {
	calls  atomic.Int32
	body   map[string]any
	header http.Header
}

// fakeAnthropic 模擬 Messages API；reply 決定回應.
func fakeAnthropic(t *testing.T, reply func(w http.ResponseWriter)) (*Claude, *claudeCapture) {
	t.Helper()
	c := &claudeCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.calls.Add(1)
		c.header = r.Header.Clone()
		data, _ := io.ReadAll(r.Body)
		c.body = map[string]any{}
		_ = json.Unmarshal(data, &c.body)
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		reply(w)
	}))
	t.Cleanup(srv.Close)
	p := NewClaude("sk-test", "", option.WithBaseURL(srv.URL), option.WithHTTPClient(srv.Client())).(*Claude)
	return p, c
}

func messageJSON(stopReason, text string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","stop_reason":"` + stopReason +
		`","content":[{"type":"text","text":"` + text + `"}],"usage":{"input_tokens":10,"output_tokens":5}}`
}

func TestClaudeRequestShape(t *testing.T) {
	p, c := fakeAnthropic(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, messageJSON("end_turn", "今天還可以吃 600 大卡"))
	})

	text, err := p.Call(context.Background(), DefaultClaudeModel, &Request{System: "規則", Prompt: "問題"})
	if err != nil || text != "今天還可以吃 600 大卡" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	// SDK v2 的 WithAPIKey 以 Authorization: Bearer 傳金鑰.
	if c.header.Get("Authorization") != "Bearer sk-test" || !strings.Contains(c.header.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("headers: auth=%q beta=%q", c.header.Get("Authorization"), c.header.Get("Anthropic-Beta"))
	}
	system, _ := c.body["system"].([]any)
	block, _ := system[0].(map[string]any)
	cache, _ := block["cache_control"].(map[string]any)
	if len(system) != 1 || block["text"] != "規則" || cache["type"] != "ephemeral" {
		t.Errorf("system block = %v", c.body["system"])
	}
	raw, _ := json.Marshal(c.body)
	for _, want := range []string{
		`"model":"claude-opus-5-5"`, `"fallbacks":"default"`, `"effort":"low"`, `"max_tokens":4096`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("body missing %s:\n%s", want, raw)
		}
	}
	if _, ok := c.body["thinking"]; ok {
		t.Error("thinking must be left unset on Opus 5.5 (adaptive is the only mode)")
	}
	if _, ok := c.body["temperature"]; ok {
		t.Error("sampling parameters are rejected by current models")
	}
}

func TestClaudeSendsImages(t *testing.T) {
	p, c := fakeAnthropic(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, messageJSON("end_turn", "ok"))
	})
	if _, err := p.Call(context.Background(), DefaultClaudeModel, &Request{Prompt: "看圖", Images: []Image{{MIME: "image/png", Data: []byte("x")}}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(c.body["messages"])
	if !strings.Contains(string(raw), `"media_type":"image/png"`) || !strings.Contains(string(raw), `"data":"eA=="`) {
		t.Errorf("image block missing: %s", raw)
	}
}

func TestClaudeErrorsMapForTheRouter(t *testing.T) {
	p, c := fakeAnthropic(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "4")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`)
	})
	_, err := p.Call(context.Background(), DefaultClaudeModel, &Request{Prompt: "p"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 429 || apiErr.RetryAfter != 4*time.Second ||
		apiErr.Message != "Number of requests has exceeded your rate limit" {
		t.Fatalf("err = %#v", err)
	}
	if c.calls.Load() != 1 {
		t.Errorf("SDK retried %d times; retries belong to the router", c.calls.Load()-1)
	}
	if wait, ok := retryWait(err); !ok || wait != 5*time.Second {
		t.Errorf("router wait = %v %v", wait, ok)
	}
}

func TestClaudeRefusalFailsOverToNextProvider(t *testing.T) {
	claude, _ := fakeAnthropic(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, messageJSON("refusal", ""))
	})
	groq := &fakeProvider{name: "groq", models: textModels("g"), script: []fakeReply{{text: "備援回答"}}}
	r, _ := newTestRouter(&Settings{}, claude, groq)

	res := r.Complete(context.Background(), &Request{Prompt: "p", Order: []string{"claude", "groq"}})

	if res.Provider != "groq" || res.Text != "備援回答" {
		t.Fatalf("got %+v", res)
	}
	if !strings.Contains(strings.Join(res.Errors, "\n"), "declined") {
		t.Errorf("refusal not recorded: %v", res.Errors)
	}
}

// Claude 是付費的：預設的文字與看圖順序都不含它，只有明確指定 Order（自由問答）才會用到，
// 就算免費供應商全掛也不會自動退到 Claude.
func TestClaudeIsNeverUsedUnlessOrdered(t *testing.T) {
	claude, c := fakeAnthropic(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, messageJSON("end_turn", "付費回答"))
	})
	down := fakeReply{err: &APIError{Status: http.StatusServiceUnavailable, Message: "overloaded"}}
	groq := &fakeProvider{
		name:   "groq",
		models: map[Capability][]string{Text: {"g"}, Vision: {"gv"}},
		script: []fakeReply{down},
	}
	r, _ := newTestRouter(&Settings{}, claude, groq)

	requests := []*Request{
		{Prompt: "天氣建議"},
		{Prompt: "看圖", Images: []Image{{Data: []byte{0xff, 0xd8}, MIME: "image/jpeg"}}},
	}
	for _, req := range requests {
		if res := r.Complete(context.Background(), req); res.OK() {
			t.Errorf("%s: got an answer from %s, want failure", req.Capability(), res.Provider)
		}
	}
	if n := c.calls.Load(); n != 0 {
		t.Errorf("Claude was called %d times without being in the order", n)
	}
	if res := r.Complete(context.Background(), &Request{Prompt: "問題", Order: []string{"claude"}}); res.Provider != "claude" {
		t.Errorf("explicit order must reach Claude, got %+v", res)
	}
}

func TestRequestOrderOverridesDefaults(t *testing.T) {
	groq := &fakeProvider{name: "groq", models: textModels("g"), script: []fakeReply{{text: "groq"}}}
	gemini := &fakeProvider{name: "gemini", models: textModels("m"), script: []fakeReply{{text: "gemini"}}}
	r, _ := newTestRouter(&Settings{TextOrder: []string{"groq", "gemini"}}, groq, gemini)
	if res := r.Complete(context.Background(), &Request{Prompt: "p", Order: []string{"gemini"}}); res.Provider != "gemini" || len(groq.calls) != 0 {
		t.Errorf("res=%+v groq calls=%v", res, groq.calls)
	}
	if got := strings.Join(r.Configured(), ","); got != "gemini,groq" {
		t.Errorf("configured = %s", got)
	}
	if NewClaude("", "", nil...) != nil {
		t.Error("no key means no provider")
	}
}

func TestSystemPromptReachesOtherProviders(t *testing.T) {
	srv, c := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ *captured) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	p := NewGroq("gsk_key", srv.Client()).(*OpenAICompatible)
	p.baseURL = srv.URL
	if _, err := p.Call(context.Background(), "openai/gpt-oss-120b", &Request{System: "規則", Prompt: "問題"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(c.body["messages"])
	if !strings.HasPrefix(string(raw), `[{"content":"規則","role":"system"}`) {
		t.Errorf("system message missing: %s", raw)
	}

	gsrv, gc := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ *captured) {
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`)
	})
	g := NewGemini("k", gsrv.Client()).(*Gemini)
	g.baseURL = gsrv.URL
	if _, err := g.Call(context.Background(), "gemini-3.8-flash", &Request{System: "規則", Prompt: "問題"}); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(gc.body["systemInstruction"])
	if string(raw) != `{"parts":[{"text":"規則"}]}` {
		t.Errorf("systemInstruction = %s", raw)
	}
}
