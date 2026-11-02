package liff

import (
	"bytes"
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

// Page 回傳統計頁（LIFF ID 寫進頁面；它是公開值）與靜態檔處理器.
func Page(liffID string) (page, static echo.HandlerFunc, err error) {
	tmpl, err := template.ParseFS(webFS, "web/index.html")
	if err != nil {
		return nil, nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]string{"LIFFID": liffID}); err != nil {
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
