package app

import (
	"chatbot-go/internal/platform/config"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// unreachableDB 連不上的 MongoDB（很快逾時），用來檢查組裝結果而不需要真的資料庫.
func unreachableDB(t *testing.T) *mongo.Database {
	t.Helper()
	client, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=100&connectTimeoutMS=100"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	return client.Database("test")
}

// postWebhook 送一個用 secret 簽章、沒有事件的 webhook.
func postWebhook(a *app, secret string) int {
	body := `{"destination":"Ubot","events":[],"secret":"` + secret + `"}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/webhooks", strings.NewReader(body))
	req.Header.Set("X-Line-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	a.echo.ServeHTTP(rec, req)
	return rec.Code
}

func get(a *app, path string) int {
	rec := httptest.NewRecorder()
	a.echo.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody))
	return rec.Code
}

// 組裝層把所有元件接起來：健康檢查反映真實依賴、LIFF 依設定開關、管理 API 有保護，關機元件都能停止.
func TestBuildWiresTheService(t *testing.T) {
	cfg := &config.Config{
		Line:     config.LineConfig{ChannelSecret: "channel-secret", ChannelToken: "channel-token"},
		Security: config.SecurityConfig{AdminToken: "admin-token-for-app-test"},
		LIFF:     config.LIFFConfig{ID: "1234567890-AbCdEfGh", ChannelID: "1234567890"},
	}
	a, err := build(cfg, unreachableDB(t), nil)
	if err != nil {
		t.Fatal(err)
	}

	if code := get(a, "/health"); code != http.StatusOK {
		t.Errorf("/health: %d", code)
	}
	if code := get(a, "/ready"); code != http.StatusServiceUnavailable {
		t.Errorf("/ready must report the unreachable MongoDB: %d", code)
	}
	if code := get(a, "/liff"); code != http.StatusOK {
		t.Errorf("/liff with LIFF configured: %d", code)
	}
	if code := get(a, "/api/v1/users"); code != http.StatusUnauthorized {
		t.Errorf("admin API without token: %d", code)
	}
	// webhook 用設定的 channel secret 驗證簽章
	for secret, want := range map[string]int{"channel-secret": http.StatusOK, "another-secret": http.StatusUnauthorized} {
		if code := postWebhook(a, secret); code != want {
			t.Errorf("webhook signed with %q: %d, want %d", secret, code, want)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range a.stoppers() {
		if err := s.Stop(ctx); err != nil {
			t.Errorf("stop %s: %v", s.Name, err)
		}
	}
}

func TestBuildWithoutLIFF(t *testing.T) {
	a, err := build(&config.Config{}, unreachableDB(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if code := get(a, "/liff"); code != http.StatusNotFound {
		t.Errorf("/liff without LIFF: %d", code)
	}
}

func TestLineConfigMapsEveryField(t *testing.T) {
	got := lineConfig(&config.LineConfig{
		ChannelSecret: "secret-not-for-the-client", ChannelToken: "token",
		ReplyURL: "reply", PushURL: "push", ContentURL: "content", LoadingURL: "loading",
	})
	if got.Token != "token" || got.ReplyURL != "reply" || got.PushURL != "push" || got.ContentURL != "content" || got.LoadingURL != "loading" {
		t.Errorf("line config = %+v", got)
	}
}
