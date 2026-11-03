package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type captured struct {
	path, query, auth, googKey string
	body                       map[string]any
}

func newServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, c *captured)) (*httptest.Server, *captured) {
	t.Helper()
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.path, c.query = r.URL.Path, r.URL.RawQuery
		c.auth, c.googKey = r.Header.Get("Authorization"), r.Header.Get("x-goog-api-key")
		data, _ := io.ReadAll(r.Body)
		c.body = nil
		_ = json.Unmarshal(data, &c.body)
		handler(w, r, c)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func TestGeminiCallSendsKeyInHeaderAndImageInline(t *testing.T) {
	srv, c := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ *captured) {
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"{\"a\":"},{"text":"1}"}]}}]}`)
	})
	g := NewGemini("AIzaSecretKey", srv.Client()).(*Gemini)
	g.baseURL = srv.URL

	text, err := g.Call(context.Background(), "gemini-3.8-flash", &Request{
		Prompt: "看圖", JSON: true, Images: []Image{{MIME: "image/jpeg", Data: []byte("img")}},
	})
	if err != nil || text != `{"a":1}` {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if c.googKey != "AIzaSecretKey" || strings.Contains(c.query, "AIza") {
		t.Errorf("key must be in header only: header=%q query=%q", c.googKey, c.query)
	}
	if c.path != "/models/gemini-3.8-flash:generateContent" {
		t.Errorf("path = %s", c.path)
	}
	raw, _ := json.Marshal(c.body)
	for _, want := range []string{`"responseMimeType":"application/json"`, `"mime_type":"image/jpeg"`, `"data":"aW1n"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("body missing %s: %s", want, raw)
		}
	}
}

func TestGeminiCandidatesRankFlashAndFallBackToAlias(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ *captured) {
		_, _ = io.WriteString(w, `{"models":[
			{"name":"models/gemini-3.7-flash","supportedGenerationMethods":["generateContent"]},
			{"name":"models/gemini-3.8-flash","supportedGenerationMethods":["generateContent"]},
			{"name":"models/gemini-3.9-flash-preview","supportedGenerationMethods":["generateContent"]},
			{"name":"models/gemini-3.8-flash-lite","supportedGenerationMethods":["generateContent"]},
			{"name":"models/gemini-3.8-flash-tts","supportedGenerationMethods":["generateContent"]},
			{"name":"models/gemini-3.1-pro","supportedGenerationMethods":["generateContent"]},
			{"name":"models/gemini-4.0-flash","supportedGenerationMethods":["embedContent"]}]}`)
	})
	g := NewGemini("k", srv.Client()).(*Gemini)
	g.baseURL = srv.URL
	got, _ := g.Candidates(context.Background(), Vision)
	want := "gemini-3.8-flash,gemini-3.7-flash,gemini-3.9-flash-preview,gemini-flash-latest"
	if strings.Join(got, ",") != want {
		t.Errorf("got %v", got)
	}

	g.baseURL = "http://127.0.0.1:1"
	if got, _ := g.Candidates(context.Background(), Text); strings.Join(got, ",") != geminiFallbackAlias {
		t.Errorf("listing failure should fall back to alias, got %v", got)
	}
}

func TestGroqCallFormatsImagesJSONModeAndLowReasoning(t *testing.T) {
	srv, c := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ *captured) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}]}`)
	})
	p := NewGroq("gsk_key", srv.Client()).(*OpenAICompatible)
	p.baseURL = srv.URL

	if _, err := p.Call(context.Background(), "openai/gpt-oss-120b", &Request{Prompt: "p", JSON: true}); err != nil {
		t.Fatal(err)
	}
	if c.auth != "Bearer gsk_key" || c.path != "/chat/completions" {
		t.Errorf("auth=%q path=%q", c.auth, c.path)
	}
	if c.body["reasoning_effort"] != "low" || c.body["response_format"] == nil {
		t.Errorf("body = %v", c.body)
	}

	if _, err := p.Call(context.Background(), "meta-llama/llama-4-scout", &Request{Prompt: "p", Images: []Image{{MIME: "image/png", Data: []byte("x")}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.body["reasoning_effort"]; ok {
		t.Error("reasoning_effort only for gpt-oss")
	}
	raw, _ := json.Marshal(c.body["messages"])
	if !strings.Contains(string(raw), `"url":"data:image/png;base64,eA=="`) {
		t.Errorf("image not sent as data URI: %s", raw)
	}
}

func TestNVIDIANoJSONModeButMaxTokens(t *testing.T) {
	srv, c := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ *captured) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	})
	p := NewNVIDIA("nvapi-key", srv.Client()).(*OpenAICompatible)
	p.baseURL = srv.URL
	if _, err := p.Call(context.Background(), "openai/gpt-oss-20b", &Request{Prompt: "p", JSON: true}); err != nil {
		t.Fatal(err)
	}
	if c.body["response_format"] != nil || c.body["reasoning_effort"] != nil || c.body["max_tokens"] != float64(4096) {
		t.Errorf("body = %v", c.body)
	}
}

func TestOpenAICandidatesByCapability(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ *captured) {
		_, _ = io.WriteString(w, `{"data":[{"id":"qwen/qwen3.8-27b"},{"id":"openai/gpt-oss-20b"},{"id":"openai/gpt-oss-120b"},
			{"id":"whisper-large-v3"},{"id":"meta-llama/llama-4-scout-17b-16e-instruct"},{"id":"meta-llama/llama-guard-4-12b"}]}`)
	})
	p := NewGroq("k", srv.Client()).(*OpenAICompatible)
	p.baseURL = srv.URL
	text, _ := p.Candidates(context.Background(), Text)
	if strings.Join(text[:2], ",") != "openai/gpt-oss-120b,openai/gpt-oss-20b" || strings.Contains(strings.Join(text, ","), "whisper") {
		t.Errorf("text = %v", text)
	}
	vision, _ := p.Candidates(context.Background(), Vision)
	// [F23-01] qwen3.8-27b 是 Groq 官方看圖模型，排在舊 llama-4 前面.
	if strings.Join(vision, ",") != "qwen/qwen3.8-27b,meta-llama/llama-4-scout-17b-16e-instruct" {
		t.Errorf("vision = %v", vision)
	}
}

// [F23-01] 純文字的 qwen 不能進看圖候選.
func TestGroqVisionExcludesTextOnlyQwen(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ *captured) {
		_, _ = io.WriteString(w, `{"data":[{"id":"qwen/qwen3-32b"},{"id":"qwen/qwen3.8-27b"}]}`)
	})
	p := NewGroq("k", srv.Client()).(*OpenAICompatible)
	p.baseURL = srv.URL
	vision, err := p.Candidates(context.Background(), Vision)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(vision, ",") != "qwen/qwen3.8-27b" {
		t.Errorf("vision = %v", vision)
	}
}

func TestAPIErrorParsing(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request, _ *captured) {
		switch r.URL.Path {
		case "/groq":
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"Rate limit reached","type":"tokens"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"status":404,"detail":"Function not found"}`)
		}
	})
	var out struct{}
	err := doJSON(context.Background(), srv.Client(), http.MethodPost, srv.URL+"/groq", nil, map[string]string{}, &out)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 429 || apiErr.Message != "Rate limit reached" || apiErr.RetryAfter != 3*time.Second {
		t.Errorf("got %#v", err)
	}
	if wait, ok := retryWait(err); !ok || wait != 4*time.Second {
		t.Errorf("wait = %v %v", wait, ok)
	}
	err = doJSON(context.Background(), srv.Client(), http.MethodGet, srv.URL+"/nv", nil, nil, &out)
	if !errors.As(err, &apiErr) || apiErr.Message != "Function not found" || unusableFor(err) != notFoundCooldown {
		t.Errorf("got %#v", err)
	}
}

func TestShortErrorTimeout(t *testing.T) {
	if got := shortError("nvidia", "m", context.DeadlineExceeded); got != "nvidia(m): 逾時無回應" {
		t.Errorf("got %q", got)
	}
}

func TestRankModelsPrefersFamilyThenSize(t *testing.T) {
	got := rankModels([]string{"x/llama-3.1-8b", "x/llama-3.3-70b", "y/gpt-oss-20b", "z/unknown", "x/embed-1b"},
		&rankRules{families: []string{"gpt-oss", "llama"}, skip: []string{"embed"}})
	if strings.Join(got, ",") != "y/gpt-oss-20b,x/llama-3.3-70b,x/llama-3.1-8b,z/unknown" {
		t.Errorf("got %v", got)
	}
}
