// Package reqid [F37-08] 每個 LINE 事件與每個 LIFF HTTP 請求一個隨機請求代號，讓同一則訊息的步驟 log 與 DB 寫入 log 串得起來.
// 代號只由 crypto/rand 產生，不從任何輸入（使用者、訊息）推導，也不帶穩定識別碼，不能用來跨訊息串同一個人.
package reqid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
)

// Unknown 取亂數失敗時的代號；訊息照常處理.
const Unknown = "unknown"

type ctxKey struct{}

// randRead 供測試注入亂數失敗.
var randRead = rand.Read

// New 回傳 16 個小寫 hex 字元（8 bytes 亂數）；crypto/rand 失敗時回 Unknown，絕不 panic.
func New() string {
	var b [8]byte
	if _, err := randRead(b[:]); err != nil {
		return Unknown
	}
	return hex.EncodeToString(b[:])
}

// With 回傳帶一組新請求代號的 ctx（context.WithoutCancel 之後仍保留）.
func With(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, New())
}

// From 取出 ctx 內的請求代號.
func From(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(ctxKey{}).(string)
	return v, ok && v != ""
}

// Attr 回傳 slog 的 req 欄位；ctx 沒有代號時回空 Attr（slog 會略過，log 行不會出現 req 鍵）.
func Attr(ctx context.Context) slog.Attr {
	if v, ok := From(ctx); ok {
		return slog.String("req", v)
	}
	return slog.Attr{}
}
