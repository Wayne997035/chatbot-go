package integration

import (
	"chatbot-go/internal/assistant/recordedit"
	"chatbot-go/internal/storage/database/pendingedit"
	"chatbot-go/internal/storage/storagetest"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func newPendingForIT(t *testing.T, expires time.Time) *recordedit.Pending {
	t.Helper()
	nonce, err := recordedit.NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	return &recordedit.Pending{
		Nonce: nonce, Kind: recordedit.OutcomeProposal, ExpiresAt: expires,
		Proposal: &recordedit.Proposal{Kind: recordedit.KindMeal, Op: recordedit.OpSetItemKcal, RecordID: "m1", Summary: "要改成 210 大卡嗎？"},
	}
}

// [F33-21] MongoDB 實作通過與記憶體、Redis 相同的契約（A40；含 20 個 goroutine 同時 Take）.
func TestMongoPendingStore(t *testing.T) {
	storagetest.PendingStoreContract(t, func(t *testing.T) recordedit.Store {
		return pendingedit.NewMongoStore(getTestDB(t))
	})
}

// [F33-21] A42：expiresAt 上有 TTL 索引（expireAfterSeconds==0）；一個 user 一個文件（連 Put 3 次只有 1 筆，_id 就是 userID）.
func TestMongoPendingStoreTTLIndex(t *testing.T) {
	db := getTestDB(t)
	s := pendingedit.NewMongoStore(db)
	coll := db.Collection(pendingedit.CollectionName)

	specs, err := coll.Indexes().ListSpecifications(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sp := range specs {
		var keys bson.D
		if err := bson.Unmarshal(sp.KeysDocument, &keys); err != nil {
			t.Fatal(err)
		}
		if len(keys) == 1 && keys[0].Key == "expiresAt" {
			found = true
			if sp.ExpireAfterSeconds == nil || *sp.ExpireAfterSeconds != 0 {
				t.Errorf("expiresAt index expireAfterSeconds = %v, want 0", sp.ExpireAfterSeconds)
			}
		}
	}
	if !found {
		t.Fatalf("no expiresAt index: %+v", specs)
	}

	far := time.Now().Add(time.Hour)
	for range 3 {
		if err := s.Put(context.Background(), "U1", newPendingForIT(t, far)); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := coll.CountDocuments(context.Background(), bson.M{}); err != nil || n != 1 {
		t.Fatalf("documents after 3 puts = %d err=%v, want 1", n, err)
	}
	if n, _ := coll.CountDocuments(context.Background(), bson.M{"_id": "U1"}); n != 1 {
		t.Errorf("the document is not keyed by userID: %d", n)
	}
}

// [F33-21] A42：TTL 清理尚未跑時（文件還在集合裡但 expiresAt 已過）Take 仍是 ErrNotFound——正確性不靠 TTL 背景執行緒.
// 文件的 data 內 Pending.ExpiresAt 是未來，所以只有 Take 的查詢條件（expiresAt > now）擋得住它.
func TestMongoPendingStoreExpiredDocNotTaken(t *testing.T) {
	db := getTestDB(t)
	s := pendingedit.NewMongoStore(db)
	coll := db.Collection(pendingedit.CollectionName)
	ctx := context.Background()

	// expiresAt 離現在 1 小時（TTL 執行緒不會在測試期間清掉它），Take 傳入的 now 在它之後.
	docExpires := time.Now().Add(time.Hour).Truncate(time.Second)
	p := newPendingForIT(t, docExpires.Add(24*time.Hour)) // data 內的 ExpiresAt 在未來
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coll.InsertOne(ctx, bson.M{
		"_id": "U1", "nonce": p.Nonce, "data": string(data), "expiresAt": docExpires, "createTime": time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Take(ctx, "U1", p.Nonce, docExpires.Add(time.Second))
	if got != nil || !errors.Is(err, recordedit.ErrNotFound) {
		t.Fatalf("an expired document was taken: %+v %v", got, err)
	}
	if n, _ := coll.CountDocuments(ctx, bson.M{"_id": "U1"}); n != 1 {
		t.Errorf("a not-found take must not delete the document: count=%d", n)
	}
	// 同一份文件在 now 還沒到 expiresAt 時取得到，證明上面是被「過期」擋下、不是別的原因.
	if got, err := s.Take(ctx, "U1", p.Nonce, docExpires.Add(-time.Second)); err != nil || got == nil {
		t.Fatalf("the same document before expiry: %+v %v", got, err)
	}
}

// [F33-21] A42：Take 成功後文件確實被刪（單次使用的證據在儲存層）.
func TestMongoPendingStoreTakeDeletesDoc(t *testing.T) {
	db := getTestDB(t)
	s := pendingedit.NewMongoStore(db)
	coll := db.Collection(pendingedit.CollectionName)
	ctx := context.Background()
	p := newPendingForIT(t, time.Now().Add(time.Hour))
	if err := s.Put(ctx, "U1", p); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Take(ctx, "U1", p.Nonce, time.Now()); err != nil || got == nil {
		t.Fatalf("take: %+v %v", got, err)
	}
	if n, err := coll.CountDocuments(ctx, bson.M{}); err != nil || n != 0 {
		t.Fatalf("documents after take = %d err=%v, want 0", n, err)
	}
}

// [F33-21] A42：索引建立失敗（模擬：同一個 key 先建了不同選項的索引 → 衝突）只 Warn，Put / Take 仍可用.
func TestMongoPendingStoreIndexConflictStillWorks(t *testing.T) {
	db := getTestDB(t)
	ctx := context.Background()
	coll := db.Collection(pendingedit.CollectionName)
	if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "expiresAt", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(3600),
	}); err != nil {
		t.Fatal(err)
	}
	s := pendingedit.NewMongoStore(db) // 內部建 expireAfterSeconds=0 的同 key 索引 → 衝突 → 只 Warn
	p := newPendingForIT(t, time.Now().Add(time.Hour))
	if err := s.Put(ctx, "U1", p); err != nil {
		t.Fatalf("put after a failed index creation: %v", err)
	}
	if got, err := s.Take(ctx, "U1", p.Nonce, time.Now()); err != nil || got == nil {
		t.Fatalf("take after a failed index creation: %+v %v", got, err)
	}
}
