package liff

import (
	"bytes"
	"chatbot-go/internal/ai"
	"embed"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/labstack/echo/v4"
)

//go:embed web
var webFS embed.FS

// contentSecurityPolicy 只允許本站、LIFF SDK 與 Google Fonts（圖表自己畫 SVG，不載入第三方圖表套件）.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' https://static.line-scdn.net; " +
	"style-src 'self' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; " +
	"connect-src 'self' https://*.line.me https://*.line-scdn.net; img-src 'self' data: https://*.line-scdn.net; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// providerNames AI 供應商代號 → 告知畫面的顯示名稱；不在表內的代號不顯示（不把未知字串寫進告知）.
var providerNames = map[string]string{
	"gemini": "Gemini",
	"groq":   "Groq",
	"nvidia": "NVIDIA",
	"claude": "Claude",
}

// PageOption 調整統計頁的渲染資料.
type PageOption func(*pageData)

type pageData struct {
	LIFFID      string
	AIProviders []string
}

// WithAIProviders 帶入目前有設金鑰的 AI 供應商代號（router.Configured()）；渲染時轉成顯示名稱，未知代號丟棄.
func WithAIProviders(names []string) PageOption {
	return func(d *pageData) {
		d.AIProviders = d.AIProviders[:0]
		for _, n := range names {
			if display, ok := providerNames[n]; ok {
				d.AIProviders = append(d.AIProviders, display)
			}
		}
	}
}

// WithAIRouter 以 AI router 目前有設金鑰的供應商當名單；router 為 nil（金鑰都沒設，AI 停用）時名單為空.
func WithAIRouter(r *ai.Router) PageOption {
	if r == nil {
		return WithAIProviders(nil)
	}
	return WithAIProviders(r.Configured())
}

// Page 回傳統計頁（LIFF ID 寫進頁面；它是公開值）與靜態檔處理器.
func Page(liffID string, opts ...PageOption) (page, static echo.HandlerFunc, err error) {
	tmpl, err := template.ParseFS(webFS, "web/index.html")
	if err != nil {
		return nil, nil, err
	}
	data := &pageData{LIFFID: liffID}
	for _, opt := range opts {
		opt(data)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, nil, err
	}
	html := buf.Bytes()
	sub, err := fs.Sub(webFS, "web/static")
	if err != nil {
		return nil, nil, err
	}
	files := http.StripPrefix("/liff/static/", http.FileServer(http.FS(sub)))

	page = func(c echo.Context) error {
		securityHeaders(c)
		c.Response().Header().Set("Cache-Control", "no-cache")
		return c.HTMLBlob(http.StatusOK, html)
	}
	static = func(c echo.Context) error {
		securityHeaders(c)
		c.Response().Header().Set("Cache-Control", "public, max-age=300")
		files.ServeHTTP(c.Response(), c.Request())
		return nil
	}
	return page, static, nil
}

func securityHeaders(c echo.Context) {
	h := c.Response().Header()
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

// NoStore API 回應不快取（個人健康資料）.
func NoStore(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-store")
		c.Response().Header().Set("X-Content-Type-Options", "nosniff")
		return next(c)
	}
}
