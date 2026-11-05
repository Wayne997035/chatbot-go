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
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
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

// recordingHook [F24-81] 攔下 Redis 指令、不連網路.
type recordingHook struct {
	mu   sync.Mutex
	cmds [][]any
}

func (r *recordingHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (r *recordingHook) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.cmds = append(r.cmds, cmd.Args())
		return nil
	}
}

func (r *recordingHook) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(context.Context, []redis.Cmder) error { return nil }
}

func (r *recordingHook) commands() [][]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]any(nil), r.cmds...)
}

func hookedRedis(t *testing.T) (*redis.Client, *recordingHook) {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = client.Close() })
	hook := &recordingHook{}
	client.AddHook(hook)
	return client, hook
}

// [F24-81] 保活寫固定 key、值只有時間、1 小時到期；Redis 沒連上不啟用.
func TestRedisKeepAliveWritesFixedKey(t *testing.T) {
	if redisKeepAlive(nil) != nil {
		t.Fatal("nil redis client must disable keep-alive")
	}
	client, hook := hookedRedis(t)
	if err := redisKeepAlive(client)(context.Background()); err != nil {
		t.Fatal(err)
	}
	cmds := hook.commands()
	if len(cmds) != 1 {
		t.Fatalf("commands = %v, want exactly one", cmds)
	}
	args := cmds[0]
	if len(args) != 5 || args[0] != "set" || args[1] != "keepalive:health" {
		t.Fatalf("args = %v", args)
	}
	if v, ok := args[2].(string); !ok {
		t.Errorf("value type %T", args[2])
	} else if _, err := time.Parse(time.RFC3339, v); err != nil {
		t.Errorf("value %q is not a timestamp: %v", v, err)
	}
	if args[3] != "ex" || args[4] != int64(3600) {
		t.Errorf("expiry args = %v %v (%T), want ex 3600", args[3], args[4], args[4])
	}
}

// [F24-81] 組裝層確實把保活接上 /health.
func TestBuildHealthKeepsRedisAlive(t *testing.T) {
	cfg := &config.Config{
		Line:     config.LineConfig{ChannelSecret: "channel-secret", ChannelToken: "channel-token"},
		Security: config.SecurityConfig{AdminToken: "admin-token-for-app-test"},
		LIFF:     config.LIFFConfig{ID: "1234567890-AbCdEfGh", ChannelID: "1234567890"},
	}
	client, hook := hookedRedis(t)
	a, err := build(cfg, unreachableDB(t), client)
	if err != nil {
		t.Fatal(err)
	}
	if code := get(a, "/health"); code != http.StatusOK {
		t.Fatalf("/health: %d", code)
	}
	// 保活在背景寫入，/health 不等它；最多等 2 秒看到這筆 set.
	found := false
	for deadline := time.Now().Add(2 * time.Second); !found && time.Now().Before(deadline); {
		for _, args := range hook.commands() {
			if len(args) > 1 && args[0] == "set" && args[1] == "keepalive:health" {
				found = true
			}
		}
		if !found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !found {
		t.Errorf("no keep-alive set recorded: %v", hook.commands())
	}
}
