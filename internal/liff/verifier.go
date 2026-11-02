// Package liff 提供 LIFF 統計頁：網頁、身分驗證與資料 API.
//
// 身分一律由後端向 LINE 驗證 ID token 取得，前端自稱的使用者資料一律不採信.
package liff

import (
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
	"sync"
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

	mu    sync.Mutex
	local map[string]cachedUser
}

type cachedUser struct {
	userID  string
	expires time.Time
}

// NewVerifier 建立驗證器；channelID 為 LIFF 所屬 LINE Login channel 的 ID. cache 可為 nil.
func NewVerifier(channelID string, client *http.Client, cache *redis.Client) *Verifier {
	return &Verifier{
		channelID: channelID,
		verifyURL: DefaultVerifyURL,
		client:    client,
		cache:     cache,
		now:       time.Now,
		local:     make(map[string]cachedUser),
	}
}

type verifyResponse struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	Aud string `json:"aud"`
	Exp int64  `json:"exp"`
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
	expires := time.Unix(claims.Exp, 0)
	if claims.Iss != lineIssuer || claims.Aud != v.channelID || claims.Sub == "" || !expires.After(v.now()) {
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
		if userID, err := v.cache.Get(ctx, key).Result(); err == nil && userID != "" {
			return userID, true
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok := v.local[key]; ok && v.now().Before(c.expires) {
		return c.userID, true
	}
	return "", false
}

func (v *Verifier) store(ctx context.Context, key, userID string, ttl time.Duration) {
	if v.cache != nil && v.cache.Set(ctx, key, userID, ttl).Err() == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	for k, c := range v.local {
		if !now.Before(c.expires) {
			delete(v.local, k)
		}
	}
	v.local[key] = cachedUser{userID: userID, expires: now.Add(ttl)}
}

// hashToken 快取鍵只用雜湊，token 本身不落地.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
