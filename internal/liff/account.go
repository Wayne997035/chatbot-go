package liff

import (
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/redact"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	userstore "chatbot-go/internal/storage/database/user"

	"github.com/labstack/echo/v4"
)

// 會員資料（名稱 / 頭像 / email）與刪除帳號.
type (
	// ClaimsVerifier [F30-03] 向 LINE 驗證 ID token 並回傳 claims（不走快取）.
	ClaimsVerifier interface {
		VerifyClaims(ctx context.Context, idToken string) (*Claims, error)
	}
	// AccountStore [F30-03] 會員資料儲存（使用端小介面）.
	AccountStore interface {
		FindByID(ctx context.Context, id string) (*userstore.User, error)
		SaveLineProfile(ctx context.Context, userID, name, pictureURL, email string) error
		// Delete 寫入後才發現使用者已刪除資料時補刪 user 文件.
		Delete(ctx context.Context, id string) error
	}
	// AccountEraser [F30-04] 刪除使用者全部個人資料（實作：webhook.WebhookHandler.EraseAll；liff 不 import webhook）.
	AccountEraser interface {
		EraseAll(ctx context.Context, userID string) error
	}
)

// 清洗上限.
const (
	maxDisplayNameRunes = 100
	maxPictureURLBytes  = 1024
	maxEmailBytes       = 254
)

// defaultEraseTimeout [F30-04] R3 整體逾時：mark 與每個寫 Redis 的步驟各自最多 erasureOpTimeout(5s)，其餘是循序的 Mongo 刪除（毫秒級）；
// 20 秒留餘裕，且小於平台關機寬限 30 秒.
const defaultEraseTimeout = 20 * time.Second

// AccountView R1、R2 的回應；三欄永遠存在，無值為 "".
type AccountView struct {
	DisplayName string `json:"displayName"`
	PictureURL  string `json:"pictureUrl"`
	Email       string `json:"email"`
}

func eraseIncompleteResponse(c echo.Context) error {
	return c.JSON(http.StatusInternalServerError,
		httputil.ErrorWithCode(httputil.ErrorCodeEraseIncomplete, "Data erasure incomplete, please try again"))
}

func erasedResponse(c echo.Context) error {
	return c.JSON(http.StatusGone, httputil.ErrorWithCode(httputil.ErrorCodeDataErased, "Data was erased, nothing saved"))
}

func notConfigured(c echo.Context) error {
	return c.JSON(http.StatusServiceUnavailable, httputil.ErrorWithCode(httputil.ErrorCodeServerError, "Not available"))
}

// hasControl 任一字元是控制字元.
func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// cleanName [F30-03] 順序：控制字元 → TrimSpace → 長度（rune，超過截到 100）.
func cleanName(s string) string {
	if hasControl(s) {
		return ""
	}
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxDisplayNameRunes {
		return string([]rune(s)[:maxDisplayNameRunes])
	}
	return s
}

// cleanPictureURL [F30-03] 控制字元 → TrimSpace → https、host 非空、≤ 1024 byte；不合就 "".
func cleanPictureURL(s string) string {
	if hasControl(s) {
		return ""
	}
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxPictureURLBytes {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return s
}

// cleanEmail [F30-03] 控制字元 → TrimSpace → ≤ 254 byte、恰一個 @ 且前後非空；不合就 "".
func cleanEmail(s string) string {
	if hasControl(s) {
		return ""
	}
	s = strings.TrimSpace(s)
	if len(s) > maxEmailBytes || strings.Count(s, "@") != 1 {
		return ""
	}
	local, domain, _ := strings.Cut(s, "@")
	if local == "" || domain == "" {
		return ""
	}
	return s
}

// erasedNow [F30-03] 用不跟著請求取消的 ctx 查刪除標記（請求取消時仍要查得到）.
func (h *Handler) erasedNow(c echo.Context, uid string, since time.Time) bool {
	if h.erased == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request().Context()), erasureOpTimeout)
	defer cancel()
	return h.erased.ErasedSince(ctx, uid, since)
}

// SyncAccount POST /me/account/sync：用本次 ID token 驗過的 claims 存名稱 / 頭像 / email（F30-03）.
// 不讀 request body：前端自稱的名稱與 email 一律不採信.
func (h *Handler) SyncAccount(c echo.Context) error {
	if h.claims == nil || h.users == nil {
		return notConfigured(c)
	}
	token, ok := strings.CutPrefix(c.Request().Header.Get("Authorization"), "Bearer ")
	if !ok {
		return unauthorized(c)
	}
	claims, err := h.claims.VerifyClaims(c.Request().Context(), token)
	if err != nil {
		if !errors.Is(err, ErrInvalidToken) {
			slog.Warn("LIFF token verification unavailable", "error", redact.Error(err))
			return c.JSON(http.StatusServiceUnavailable, httputil.ErrorWithCode(
				httputil.ErrorCodeServerError, "Verification temporarily unavailable",
			))
		}
		return unauthorized(c)
	}
	uid := userID(c)
	if claims.Sub != uid {
		return unauthorized(c)
	}
	name, picture, email := cleanName(claims.Name), cleanPictureURL(claims.Picture), cleanEmail(claims.Email)

	// [F30-03] R1 一律以零值 since 查標記：標記有效期間（erasureTTL）內任何刪除都擋，刪除後立刻重開 LIFF 不會把資料重新建起來.
	var since time.Time
	if h.erasedNow(c, uid, since) {
		return erasedResponse(c)
	}
	saveErr := h.users.SaveLineProfile(c.Request().Context(), uid, name, picture, email)
	// 寫後再查（回錯也查，可能已落地）：命中就用不隨請求取消的 ctx 補刪 user 文件.
	if h.erasedNow(c, uid, since) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request().Context()), erasureOpTimeout)
		defer cancel()
		if err := h.users.Delete(cctx, uid); err != nil {
			slog.Error("LIFF API", "op", "undo account sync after erasure", "error", redact.Error(err))
			return eraseIncompleteResponse(c)
		}
		return erasedResponse(c)
	}
	if saveErr != nil {
		return serverError(c, "save account profile", saveErr)
	}
	return httputil.OK(c, AccountView{DisplayName: name, PictureURL: picture, Email: email})
}

// GetAccount GET /me/account：讀自己的會員資料；沒有資料回三個空字串（不是 404）.
func (h *Handler) GetAccount(c echo.Context) error {
	if h.users == nil {
		return notConfigured(c)
	}
	u, err := h.users.FindByID(c.Request().Context(), userID(c))
	if err != nil {
		return serverError(c, "load account", err)
	}
	if u == nil {
		return httputil.OK(c, AccountView{})
	}
	return httputil.OK(c, AccountView{DisplayName: u.DisplayName, PictureURL: u.PictureURL, Email: u.Email})
}

// DeleteAccount DELETE /me/account：自助刪除全部個人資料（F30-04）.
func (h *Handler) DeleteAccount(c echo.Context) error {
	if h.eraser == nil {
		return notConfigured(c)
	}
	timeout := h.eraseTimeout
	if timeout <= 0 {
		timeout = defaultEraseTimeout
	}
	// 使用者關掉頁面 / 請求取消不能把刪除做一半：脫離請求 ctx，只受整體逾時限制；逾時 → EraseAll 回錯 → 500，不得回 204.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request().Context()), timeout)
	defer cancel()
	if err := h.eraser.EraseAll(ctx, userID(c)); err != nil {
		slog.Error("LIFF API", "op", "erase account", "error", redact.Error(err))
		return eraseIncompleteResponse(c)
	}
	return c.NoContent(http.StatusNoContent)
}
