package liff

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// renderPage 以給定選項渲染 /liff，回傳 body.
func renderPage(t *testing.T, opts ...PageOption) string {
	t.Helper()
	page, _, err := Page("2001234567-AbCdEfGh", opts...)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	e := echo.New()
	e.GET("/liff", page)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/liff", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	return rec.Body.String()
}

// providerBlock 取出告知畫面中 AI 供應商名單那一段.
func providerBlock(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `id="ai-providers"`)
	if !ok {
		t.Fatal("ai-providers block missing from page")
	}
	block, _, ok := strings.Cut(rest, "</div>")
	if !ok {
		t.Fatal("ai-providers block is not closed")
	}
	return block
}

// TestPageRendersConfiguredAIProviders [F30-20] 告知畫面的 AI 供應商名單由伺服器依傳入名單渲染（AC-2a~2d）.
func TestPageRendersConfiguredAIProviders(t *testing.T) {
	const noAI = "目前未啟用 AI 分析服務，你的照片與文字不會送至第三方 AI 供應商"
	tests := []struct {
		name      string
		providers []string
		want      []string
		notWant   []string
	}{
		{name: "only gemini", providers: []string{"gemini"}, want: []string{"<li>Gemini</li>"}, notWant: []string{"Groq", "NVIDIA", "Claude", noAI}},
		{name: "production three", providers: []string{"gemini", "groq", "nvidia"}, want: []string{"<li>Gemini</li>", "<li>Groq</li>", "<li>NVIDIA</li>"}, notWant: []string{"Claude", noAI}},
		{name: "claude only when configured", providers: []string{"claude", "gemini"}, want: []string{"<li>Claude</li>", "<li>Gemini</li>"}, notWant: []string{"Groq", noAI}},
		{name: "empty list", providers: []string{}, want: []string{noAI}, notWant: []string{"<li>", "Gemini", "Groq", "NVIDIA", "Claude"}},
		{name: "nil list", providers: nil, want: []string{noAI}, notWant: []string{"<li>"}},
		{name: "unknown code is dropped", providers: []string{"evil<script>alert(1)</script>", "gemini", "GEMINI"}, want: []string{"<li>Gemini</li>"}, notWant: []string{"evil", "<script>alert", "&lt;script", noAI}},
		{name: "only unknown codes", providers: []string{"evil"}, want: []string{noAI}, notWant: []string{"evil", "<li>"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			block := providerBlock(t, renderPage(t, WithAIProviders(tt.providers)))
			for _, w := range tt.want {
				if !strings.Contains(block, w) {
					t.Errorf("block missing %q:\n%s", w, block)
				}
			}
			for _, nw := range tt.notWant {
				if strings.Contains(block, nw) {
					t.Errorf("block must not contain %q:\n%s", nw, block)
				}
			}
		})
	}
}

// TestPageWithoutOptionsAndNilRouter [F30-20] 不帶選項（既有呼叫）與 router 為 nil 都能渲染，名單為空.
func TestPageWithoutOptionsAndNilRouter(t *testing.T) {
	for name, body := range map[string]string{
		"no options": renderPage(t),
		"nil router": renderPage(t, WithAIRouter(nil)),
	} {
		if block := providerBlock(t, body); !strings.Contains(block, "目前未啟用 AI 分析服務") || strings.Contains(block, "<li>") {
			t.Errorf("%s: block = %s", name, block)
		}
		if !strings.Contains(body, `data-liff-id="2001234567-AbCdEfGh"`) {
			t.Errorf("%s: LIFF ID missing", name)
		}
	}
}

// TestPageHasNoUnrenderedPlaceholders [F30-20] 渲染後的頁面不留任何模板或規格佔位符.
func TestPageHasNoUnrenderedPlaceholders(t *testing.T) {
	body := renderPage(t, WithAIProviders([]string{"gemini"}))
	for _, bad := range []string{"{{", "}}", "{OPERATOR", "OPERATOR_NAME", "TODO"} {
		if strings.Contains(body, bad) {
			t.Errorf("rendered page contains %q", bad)
		}
	}
}
