package pendingedit

import (
	"chatbot-go/internal/assistant/recordedit"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// CollectionName [F33-21] 暫存集合名稱（沒有 Redis 時使用；無 FK、無任何跨集合參照，刪除資料時由 eraseUserData 明確呼叫 Delete）.
const CollectionName = "pending_edit"

// indexTimeout 建索引的時間上限.
const indexTimeout = 10 * time.Second

// doc 一個 user 一個文件（_id 就是 userID，所以查詢條件天生只取發送者自己的）.
type doc struct {
	ID         string    `bson:"_id"`
	Nonce      string    `bson:"nonce"`
	Data       string    `bson:"data"` // recordedit.Pending 的 JSON
	ExpiresAt  time.Time `bson:"expiresAt"`
	CreateTime time.Time `bson:"createTime"`
}

// MongoStore recordedit.Store 的 MongoDB 實作.
type MongoStore struct {
	coll *mongo.Collection
}

var _ recordedit.Store = (*MongoStore)(nil)

// NewMongoStore 建立 MongoDB 版暫存；建構時建 expiresAt 的 TTL 索引（expireAfterSeconds=0），失敗只 Warn、不擋啟動.
// TTL 索引只當清理用：MongoDB 官方文件指出背景刪除每 60 秒跑一次、過期文件可能殘留
// （https://www.mongodb.com/docs/manual/core/index-ttl/），所以過期的正確性在 Take 的查詢條件，不靠它.
func NewMongoStore(db *mongo.Database) *MongoStore {
	coll := db.Collection(CollectionName)
	ctx, cancel := context.WithTimeout(context.Background(), indexTimeout)
	defer cancel()
	index := mongo.IndexModel{
		Keys:    bson.D{{Key: "expiresAt", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0),
	}
	if _, err := coll.Indexes().CreateOne(ctx, index); err != nil {
		slog.Warn("pending_edit ttl index creation", "error", err)
	}
	return &MongoStore{coll: coll}
}

// Backend 後端種類.
func (s *MongoStore) Backend() string { return BackendMongo }

// Put 覆蓋該使用者既有的項目（ReplaceOne upsert：每人一筆，舊的 nonce 隨之失效）.
func (s *MongoStore) Put(ctx context.Context, userID string, p *recordedit.Pending) error {
	if err := validPut(userID, p); err != nil {
		return err
	}
	data, err := encode(p)
	if err != nil {
		return err
	}
	d := doc{ID: userID, Nonce: p.Nonce, Data: data, ExpiresAt: p.ExpiresAt, CreateTime: time.Now()}
	if _, err := s.coll.ReplaceOne(ctx, bson.M{"_id": userID}, d, options.Replace().SetUpsert(true)); err != nil {
		return fmt.Errorf("put pending edit: %w", err)
	}
	return nil
}

// Take 單一原子操作：比對 _id（userID）與 nonce、expiresAt > now、取出並刪除；不匹配不刪.
// 過期只由這條查詢判斷（不在 Go 端另比 Pending.ExpiresAt）.
func (s *MongoStore) Take(ctx context.Context, userID, nonce string, now time.Time) (*recordedit.Pending, error) {
	if userID == "" || !recordedit.ValidNonce(nonce) {
		return nil, recordedit.ErrNotFound
	}
	filter := bson.M{"_id": userID, "nonce": nonce, "expiresAt": bson.M{"$gt": now}}
	var d doc
	if err := s.coll.FindOneAndDelete(ctx, filter).Decode(&d); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, recordedit.ErrNotFound
		}
		return nil, fmt.Errorf("take pending edit: %w", err)
	}
	return decode(d.Data)
}

// Delete 刪除該使用者的項目；不存在不算錯.
func (s *MongoStore) Delete(ctx context.Context, userID string) error {
	if userID == "" {
		return nil
	}
	if _, err := s.coll.DeleteOne(ctx, bson.M{"_id": userID}); err != nil {
		return fmt.Errorf("delete pending edit: %w", err)
	}
	return nil
}
