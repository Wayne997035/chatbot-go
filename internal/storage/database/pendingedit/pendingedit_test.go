package pendingedit

import (
	"chatbot-go/internal/assistant/recordedit"
	"chatbot-go/internal/storage/storagetest"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var ctx = context.Background()

func newMiniRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return mr, c
}

func newPending(t *testing.T, expires time.Time) *recordedit.Pending {
	t.Helper()
	nonce, err := recordedit.NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	return &recordedit.Pending{
		Nonce: nonce, Kind: recordedit.OutcomeProposal, ExpiresAt: expires,
		Proposal: &recordedit.Proposal{Kind: recordedit.KindMeal, Op: recordedit.OpSetItemKcal, RecordID: "m1", Summary: "要改成 210 大卡嗎？", Before: 120, After: 210},
	}
}

// [F33-21] Redis 實作通過與記憶體、MongoDB 相同的契約（A40；含 20 個 goroutine 同時 Take）.
func TestRedisPendingStore(t *testing.T) {
	storagetest.PendingStoreContract(t, func(t *testing.T) recordedit.Store {
		_, c := newMiniRedis(t)
		return NewRedisStore(c)
	})
}

// [F33-21] A41：miniredis 快轉 5 分 1 秒後 key 已過期；4 分 59 秒仍在，且 key 的 TTL 是 300 秒.
func TestRedisPendingStoreExpiry(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		advance time.Duration
		wantHit bool
	}{
		{"4m59s still there", 4*time.Minute + 59*time.Second, true},
		{"5m01s expired by Redis TTL", 5*time.Minute + time.Second, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr, c := newMiniRedis(t)
			s := NewRedisStore(c)
			p := newPending(t, now.Add(recordedit.PendingTTL))
			if err := s.Put(ctx, "U1", p); err != nil {
				t.Fatal(err)
			}
			if ttl := mr.TTL("pedit:U1"); ttl != recordedit.PendingTTL {
				t.Fatalf("key TTL = %v, want %v", ttl, recordedit.PendingTTL)
			}
			mr.FastForward(tt.advance)
			// now 跟著快轉的時間走（呼叫端的時鐘），key 是否還在由 Redis 決定.
			got, err := s.Take(ctx, "U1", p.Nonce, now.Add(tt.advance))
			if tt.wantHit && (err != nil || got == nil) {
				t.Fatalf("want a hit: %+v %v", got, err)
			}
			if !tt.wantHit && (got != nil || !errors.Is(err, recordedit.ErrNotFound)) {
				t.Fatalf("want not-found: %+v %v", got, err)
			}
			if !tt.wantHit && mr.Exists("pedit:U1") {
				t.Error("expired key is still in Redis")
			}
		})
	}
}

// [F33-21] A41：Lua 腳本在 nonce 不符時 key 與 TTL 都不動；之後正確 nonce 仍可取.
func TestRedisTakeWrongNonceKeepsKey(t *testing.T) {
	mr, c := newMiniRedis(t)
	s := NewRedisStore(c)
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	p := newPending(t, now.Add(recordedit.PendingTTL))
	if err := s.Put(ctx, "U1", p); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(100 * time.Second)
	ttlBefore := mr.TTL("pedit:U1")
	wrong := "AAAAAAAAAAAAAAAAAAAAAA"
	if wrong == p.Nonce {
		t.Fatal("test nonce collides")
	}
	if got, err := s.Take(ctx, "U1", wrong, now); got != nil || !errors.Is(err, recordedit.ErrNotFound) {
		t.Fatalf("wrong nonce: %+v %v", got, err)
	}
	if !mr.Exists("pedit:U1") {
		t.Fatal("a wrong nonce deleted the valid pending")
	}
	if ttlAfter := mr.TTL("pedit:U1"); ttlAfter != ttlBefore {
		t.Errorf("TTL changed by a wrong nonce: %v -> %v", ttlBefore, ttlAfter)
	}
	if got, err := s.Take(ctx, "U1", p.Nonce, now); err != nil || got == nil {
		t.Fatalf("valid nonce after a wrong one: %+v %v", got, err)
	}
	if mr.Exists("pedit:U1") {
		t.Error("take did not delete the key")
	}
}

// [F33-25][F33-21] A43：啟動時依 Redis 有沒有連上選後端（app.go 的 WithRecordEdit(pendingedit.Select(...)) 靠它）.
func TestSelectChoosesBackend(t *testing.T) {
	_, c := newMiniRedis(t)
	mongoStore := &MongoStore{}
	tests := []struct {
		name  string
		redis *redis.Client
		want  string
	}{
		{"no redis uses mongo", nil, BackendMongo},
		{"redis present uses redis", c, BackendRedis},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Select(tt.redis, mongoStore)
			if BackendOf(got) != tt.want {
				t.Fatalf("backend = %q, want %q", BackendOf(got), tt.want)
			}
			if tt.want == BackendMongo && got != recordedit.Store(mongoStore) {
				t.Error("Select(nil, m) must return m itself")
			}
		})
	}
	if BackendOf(storagetest.NewPendingEdits()) != "" {
		t.Error("a foreign store has no backend name")
	}
}

// [F33-21] A43：後端執行期不可用時 Put / Take 回 error（不是 ErrNotFound），也不偷偷退回另一個後端.
func TestPendingStoreRuntimeFailureFailsClosed(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	p := newPending(t, now.Add(recordedit.PendingTTL))

	// Redis：連上之後關掉.
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, DialTimeout: 300 * time.Millisecond})
	t.Cleanup(func() { _ = c.Close() })
	redisStore := NewRedisStore(c)
	mr.Close()

	// Mongo：指向沒有人在聽的埠，選伺服器逾時設很短.
	client, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1").SetServerSelectionTimeout(300 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(ctx) })
	mongoStore := NewMongoStore(client.Database("pedit_down"))

	for name, s := range map[string]recordedit.Store{"redis": redisStore, "mongo": mongoStore} {
		t.Run(name, func(t *testing.T) {
			if err := s.Put(ctx, "U1", p); err == nil {
				t.Error("Put on a dead backend must fail")
			}
			got, err := s.Take(ctx, "U1", p.Nonce, now)
			if got != nil || err == nil || errors.Is(err, recordedit.ErrNotFound) {
				t.Errorf("Take on a dead backend must be a real error, not not-found: %+v %v", got, err)
			}
			if err := s.Delete(ctx, "U1"); err == nil {
				t.Error("Delete on a dead backend must fail")
			}
		})
	}
}
