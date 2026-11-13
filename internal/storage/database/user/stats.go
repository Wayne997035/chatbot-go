package userstore

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// CountUsers [F30-78] 使用者文件總數（唯讀；缺 createTime 的舊文件也算）.
func (r *UserRepo) CountUsers(ctx context.Context) (int64, error) {
	n, err := r.collection.CountDocuments(ctx, bson.M{})
	if err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// CountUsersCreatedSince [F30-78] createTime >= since 的人數；缺 createTime 的文件不符合條件所以不計入.
func (r *UserRepo) CountUsersCreatedSince(ctx context.Context, since time.Time) (int64, error) {
	n, err := r.collection.CountDocuments(ctx, bson.M{"createTime": bson.M{"$gte": since}})
	if err != nil {
		return 0, fmt.Errorf("count users since: %w", err)
	}
	return n, nil
}

// ListUsersPage [F30-78] 依 createTime 降冪、_id 升冪分頁（_id 是 tiebreak，翻頁不重複不漏；缺 createTime 的排最後）.
func (r *UserRepo) ListUsersPage(ctx context.Context, offset, limit int) ([]User, error) {
	opts := options.Find().
		SetSort(bson.D{{Key: "createTime", Value: -1}, {Key: "_id", Value: 1}}).
		SetSkip(int64(offset)).
		SetLimit(int64(limit))
	cursor, err := r.collection.Find(ctx, bson.M{}, opts)
	if err != nil {
		return nil, fmt.Errorf("list users page: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var users []User
	if err := cursor.All(ctx, &users); err != nil {
		return nil, fmt.Errorf("decode users page: %w", err)
	}
	return users, nil
}
