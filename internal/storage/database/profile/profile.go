// Package profile 使用者身體資料（計算熱量用）.
package profile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// 性別.
const (
	SexMale   = "male"
	SexFemale = "female"
)

// Profile 身體資料；0 / 空字串表示未設定.
type Profile struct {
	UserID     string    `bson:"_id"`
	WeightKG   float64   `bson:"weightKg,omitempty"`
	HeightCM   float64   `bson:"heightCm,omitempty"`
	Age        int       `bson:"age,omitempty"`
	Sex        string    `bson:"sex,omitempty"`
	UpdateTime time.Time `bson:"updateTime"`
}

// ProfileRepository 身體資料儲存庫介面.
type ProfileRepository interface {
	// Get 找不到時回傳 nil, nil.
	Get(ctx context.Context, userID string) (*Profile, error)
	// Upsert 整筆取代（LIFF 表單一次送出所有欄位）.
	Upsert(ctx context.Context, p *Profile) error
	// Merge 只更新 patch 裡有值的欄位（0 / 空字串表示沒提到），沒有資料時建立；回傳更新後的完整資料.
	Merge(ctx context.Context, patch *Profile) (*Profile, error)
	Delete(ctx context.Context, userID string) error
}

// ProfileRepo 身體資料儲存庫實現.
type ProfileRepo struct {
	collection *mongo.Collection
}

// NewProfileRepository 建立身體資料儲存庫.
func NewProfileRepository(db *mongo.Database) *ProfileRepo {
	return &ProfileRepo{collection: db.Collection("profile")}
}

// Get 取得身體資料.
func (r *ProfileRepo) Get(ctx context.Context, userID string) (*Profile, error) {
	var p Profile
	if err := r.collection.FindOne(ctx, bson.M{"_id": userID}).Decode(&p); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, fmt.Errorf("get profile: %w", err)
	}
	return &p, nil
}

// Upsert 建立或整筆更新身體資料.
func (r *ProfileRepo) Upsert(ctx context.Context, p *Profile) error {
	_, err := r.collection.ReplaceOne(ctx, bson.M{"_id": p.UserID}, p, options.Replace().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("upsert profile: %w", err)
	}
	return nil
}

// Merge 用 $set 原子地只更新有提到的欄位：連續傳「體重 70」「身高 175」兩則訊息同時處理時，
// 兩個欄位都會留下（先讀再整筆寫回的話，後寫的會蓋掉先寫的）.
func (r *ProfileRepo) Merge(ctx context.Context, patch *Profile) (*Profile, error) {
	set := bson.M{"updateTime": patch.UpdateTime}
	if patch.WeightKG > 0 {
		set["weightKg"] = patch.WeightKG
	}
	if patch.HeightCM > 0 {
		set["heightCm"] = patch.HeightCM
	}
	if patch.Age > 0 {
		set["age"] = patch.Age
	}
	if patch.Sex != "" {
		set["sex"] = patch.Sex
	}
	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
	var p Profile
	if err := r.collection.FindOneAndUpdate(ctx, bson.M{"_id": patch.UserID}, bson.M{"$set": set}, opts).Decode(&p); err != nil {
		return nil, fmt.Errorf("merge profile: %w", err)
	}
	return &p, nil
}

// Delete 刪除身體資料.
func (r *ProfileRepo) Delete(ctx context.Context, userID string) error {
	if _, err := r.collection.DeleteOne(ctx, bson.M{"_id": userID}); err != nil {
		return fmt.Errorf("delete profile: %w", err)
	}
	return nil
}
