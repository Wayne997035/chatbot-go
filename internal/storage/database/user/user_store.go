package userstore

import (
	"context"
	"fmt"
	"log/slog"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// UserRepository 使用者儲存庫介面.
type UserRepository interface {
	FindByID(ctx context.Context, id string) (*User, error)
	FindAll(ctx context.Context) ([]User, error)
	Upsert(ctx context.Context, user *User) error
	// Delete 刪除使用者紀錄（刪除個人資料時使用）.
	Delete(ctx context.Context, id string) error
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

func (r *UserRepo) FindAll(ctx context.Context) ([]User, error) {
	cursor, err := r.collection.Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cursor.Close(ctx) }()

	var users []User
	if err := cursor.All(ctx, &users); err != nil {
		return nil, err
	}
	return users, nil
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
		slog.Error("user upsert failed", "userID", user.ID, "error", err)
		return err
	}
	return nil
}
