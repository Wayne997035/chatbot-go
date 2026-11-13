package userstore

import (
	"chatbot-go/internal/redact"
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// UserRepository 使用者儲存庫介面.
type UserRepository interface {
	FindByID(ctx context.Context, id string) (*User, error)
	Upsert(ctx context.Context, user *User) error
	// Delete 刪除使用者紀錄（刪除個人資料時使用）.
	Delete(ctx context.Context, id string) error
	// SaveLineProfile [F30-03] 以驗過的 claims 覆寫名稱 / 頭像（空字串也明確寫入）並清掉舊版殘留的 email 欄；文件不存在時建立（不設 type）.
	SaveLineProfile(ctx context.Context, userID, name, pictureURL string) error
}

// UserRepo 使用者儲存庫實現.
type UserRepo struct {
	collection *mongo.Collection
}

// NewUserRepository 建立使用者儲存庫.
func NewUserRepository(db *mongo.Database) *UserRepo {
	return &UserRepo{
		collection: db.Collection("user"),
	}
}

func (r *UserRepo) FindByID(ctx context.Context, id string) (*User, error) {
	var user User
	err := r.collection.FindOne(ctx, bson.M{"_id": id}).Decode(&user)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

// Delete 刪除使用者紀錄.
func (r *UserRepo) Delete(ctx context.Context, id string) error {
	if _, err := r.collection.DeleteOne(ctx, bson.M{"_id": id}); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return nil
}

func (r *UserRepo) Upsert(ctx context.Context, user *User) error {
	filter := bson.M{"_id": user.ID}
	update := bson.M{"$setOnInsert": user}
	opts := options.UpdateOne().SetUpsert(true)

	_, err := r.collection.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		logUpsertError(err)
		return err
	}
	return nil
}

// lineProfileUpdate [F30-03] SaveLineProfile 的更新內容：名稱 / 頭像一律 $set（claim 不存在 = 清成空字串，舊值不殘留）；createTime 只在新建時寫.
// [F31-01] 不蒐集信箱：email 欄一律 $unset（舊版同步寫過的文件在下次同步時清掉；欄位本來就不存在時是 no-op）.
func lineProfileUpdate(name, pictureURL string, now time.Time) bson.M {
	return bson.M{
		"$set":         bson.M{"displayName": name, "pictureUrl": pictureURL},
		"$unset":       bson.M{"email": ""},
		"$setOnInsert": bson.M{"createTime": now},
	}
}

// SaveLineProfile 見 UserRepository. 錯誤只帶操作名，不帶 userID 與使用者資料. 真 Mongo 行為見 tests/integration/user_profile_integration_test.go [F30-28].
func (r *UserRepo) SaveLineProfile(ctx context.Context, userID, name, pictureURL string) error {
	opts := options.UpdateOne().SetUpsert(true)
	if _, err := r.collection.UpdateOne(ctx, bson.M{"_id": userID}, lineProfileUpdate(name, pictureURL, time.Now()), opts); err != nil {
		return fmt.Errorf("save line profile: %w", err)
	}
	return nil
}

// logUpsertError [F30-08] 錯誤字串可能帶連線位址或 userID，一律經 redact.
func logUpsertError(err error) {
	slog.Error("user upsert failed", "error", redact.Error(err))
}
