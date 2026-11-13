// Package liff 提供 LIFF 統計頁：網頁、身分驗證與資料 API.
//
// 身分一律由後端向 LINE 驗證 ID token 取得，前端自稱的使用者資料一律不採信.
package liff

import (
	"chatbot-go/internal/platform/localcache"
	"chatbot-go/internal/redact"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultVerifyURL LINE Login 的 ID token 驗證端點.
const DefaultVerifyURL = "https://api.line.me/oauth2/v2.1/verify"

// lineIssuer ID token 的簽發者.
const lineIssuer = "https://access.line.me"

const (
	maxTokenBytes  = 4096
	maxCacheTTL    = 30 * time.Minute
	cacheKeyPrefix = "liff:idt:"

	// [F24-45] 本地快取容量：1000 個 token 雜湊 / 最長 30 分鐘.
	localMaxSize = 1000
)

// ErrInvalidToken ID token 無效、過期或不是發給本 channel.
var ErrInvalidToken = errors.New("invalid LIFF ID token")

// Verifier 驗證 LIFF ID token，結果快取到 token 過期為止.
type Verifier struct {
	channelID string
	verifyURL string
	client    *http.Client
	cache     *redis.Client
	now       func() time.Time

	// [F24-45] Redis 沒設定或出錯時的本地快取；TTL 不超過 token 剩餘時間.
	local *localcache.Cache[cachedUser]
}

type cachedUser struct {
	userID  string
	expires time.Time
}

// NewVerifier 建立驗證器；channelID 為 LIFF 所屬 LINE Login channel 的 ID. cache 可為 nil.
func NewVerifier(channelID string, client *http.Client, cache *redis.Client) *Verifier {
	v := &Verifier{
		channelID: channelID,
		verifyURL: DefaultVerifyURL,
		client:    client,
		cache:     cache,
		now:       time.Now,
	}
	// 時鐘惰性讀 v.now，讓本地 TTL 跟著注入的時鐘.
	v.local = localcache.NewWithClock[cachedUser]("liff.idtoken", localMaxSize, maxCacheTTL, func() time.Time { return v.now() })
	return v
}

type verifyResponse struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	Aud string `json:"aud"`
	Exp int64  `json:"exp"`
	// [F30-03] 沒要求 profile scope 時 LINE 不回這兩欄，缺 = 空字串. [F31-01] 只解析這兩欄，其餘 claims 不解析.
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

// Claims [F30-03] 伺服器向 LINE 驗過的 ID token claims（個資：不進快取、不進 log）.
type Claims struct {
	Sub     string
	Name    string
	Picture string
}

// valid 共用的 iss / aud / sub / exp 檢查（Verify 與 VerifyClaims 同一份）；回傳 token 的到期時間.
func (v *Verifier) valid(r *verifyResponse) (time.Time, bool) {
	expires := time.Unix(r.Exp, 0)
	ok := r.Iss == lineIssuer && r.Aud == v.channelID && r.Sub != "" && expires.After(v.now())
	return expires, ok
}

// VerifyClaims [F30-03] 向 LINE 驗證 ID token 並回傳 claims；不讀不寫快取（名稱 / 頭像不落地到 Redis 或本地快取）.
func (v *Verifier) VerifyClaims(ctx context.Context, idToken string) (*Claims, error) {
	idToken = strings.TrimSpace(idToken)
	if idToken == "" || len(idToken) > maxTokenBytes {
		return nil, ErrInvalidToken
	}
	resp, err := v.callLINE(ctx, idToken)
	if err != nil {
		return nil, err
	}
	if _, ok := v.valid(resp); !ok {
		return nil, ErrInvalidToken
	}
	return &Claims{Sub: resp.Sub, Name: resp.Name, Picture: resp.Picture}, nil
}

// Verify 回傳 token 對應的 LINE user ID.
func (v *Verifier) Verify(ctx context.Context, idToken string) (string, error) {
	idToken = strings.TrimSpace(idToken)
	if idToken == "" || len(idToken) > maxTokenBytes {
		return "", ErrInvalidToken
	}
	key := cacheKeyPrefix + hashToken(idToken)
	if userID, ok := v.cached(ctx, key); ok {
		return userID, nil
	}

	claims, err := v.callLINE(ctx, idToken)
	if err != nil {
		return "", err
	}
	expires, ok := v.valid(claims)
	if !ok {
		return "", ErrInvalidToken
	}
	ttl := expires.Sub(v.now())
	if ttl > maxCacheTTL {
		ttl = maxCacheTTL
	}
	v.store(ctx, key, claims.Sub, ttl)
	return claims.Sub, nil
}

func (v *Verifier) callLINE(ctx context.Context, idToken string) (*verifyResponse, error) {
	form := url.Values{"id_token": {idToken}, "client_id": {v.channelID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.verifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create verify request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("verify ID token: %s", redact.Error(err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("read verify response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		return nil, ErrInvalidToken
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("LINE verify returned %d", resp.StatusCode)
	}
	var claims verifyResponse
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, fmt.Errorf("decode verify response: %w", err)
	}
	return &claims, nil
}

func (v *Verifier) cached(ctx context.Context, key string) (string, bool) {
	if v.cache != nil {
		userID, err := v.cache.Get(ctx, key).Result()
		if err == nil && userID != "" {
			return userID, true
		}
		if errors.Is(err, redis.Nil) {
			return "", false // 權威 miss，不查本地
		}
	}
	// 命中時仍檢查 expires（縱深防禦：TTL 與 token 到期任一漂移都不回舊 userID）.
	if c, ok := v.local.Get(key); ok && v.now().Before(c.expires) {
		return c.userID, true
	}
	return "", false
}

func (v *Verifier) store(ctx context.Context, key, userID string, ttl time.Duration) {
	if v.cache != nil && v.cache.Set(ctx, key, userID, ttl).Err() == nil {
		return
	}
	v.local.SetTTL(key, cachedUser{userID: userID, expires: v.now().Add(ttl)}, ttl)
}

// hashToken 快取鍵只用雜湊，token 本身不落地.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
