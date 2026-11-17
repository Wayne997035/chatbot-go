package app

import (
	"bytes"
	"chatbot-go/internal/ai"
	"chatbot-go/internal/platform/config"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// interceptTransport 攔下 newAIRouter 建的 http.Client 送出的請求（該 client 沒設 Transport，用 DefaultTransport）.
type interceptTransport struct {
	mu        sync.Mutex
	paths     []string
	models    []string
	remaining []time.Duration
}

func (it *interceptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	data, _ := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	var body struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(data, &body)
	it.mu.Lock()
	it.paths = append(it.paths, req.URL.Path)
	it.models = append(it.models, body.Model)
	if d, ok := req.Context().Deadline(); ok {
		it.remaining = append(it.remaining, time.Until(d))
	}
	it.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`)),
		Request:    req,
	}, nil
}

// [F33-33] G-01：newAIRouter 真的把 aiSettings 的結果交給 router.
// 不得 t.Parallel：暫換全域 http.DefaultTransport 與 slog 預設 logger.
func TestNewAIRouterAppliesAISettings(t *testing.T) {
	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	it := &interceptTransport{}
	oldTransport := http.DefaultTransport
	http.DefaultTransport = it
	t.Cleanup(func() {
		http.DefaultTransport = oldTransport
		slog.SetDefault(oldLogger)
	})

	router := newAIRouter(&config.AIConfig{
		GroqAPIKey: "k", GroqModels: "Groq/X", ProviderOrder: "groq", AttemptTimeoutSeconds: "3",
	})
	if router == nil {
		t.Fatal("newAIRouter returned nil although a Groq key is set")
	}

	res := router.Complete(context.Background(), &ai.Request{Prompt: "p"})
	if !res.OK() {
		t.Errorf("the intercepted request must be answered: %+v", res)
	}

	it.mu.Lock()
	defer it.mu.Unlock()
	if len(it.models) != 1 || it.models[0] != "Groq/X" {
		t.Errorf("① request model = %v, want exactly [Groq/X] (list wired, case preserved)", it.models)
	}
	for _, p := range it.paths {
		if strings.HasSuffix(p, "/models") {
			t.Errorf("② list mode must never request /models, got %v", it.paths)
		}
	}
	if len(it.remaining) != 1 || it.remaining[0] <= 0 || it.remaining[0] > 3*time.Second {
		t.Errorf("③ request ctx remaining = %v, want within (0, 3s] (AI_ATTEMPT_TIMEOUT_SECONDS=3 wired)", it.remaining)
	}
	var line string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "AI model lists") {
			line = l
		}
	}
	if !strings.Contains(line, "groq=1") || !strings.Contains(line, "nvidia=0") || strings.Contains(line, "Groq/X") {
		t.Errorf("④ startup log = %q, want counts groq=1 nvidia=0 and no list content", line)
	}
}
