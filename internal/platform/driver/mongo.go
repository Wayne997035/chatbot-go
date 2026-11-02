// Package driver 建立資料庫連線. 沒有全域狀態：連線由呼叫端（app 組裝層）持有並在結束時關閉.
package driver

import (
	"chatbot-go/internal/platform/config"
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ConnectMongo 連線並確認 MongoDB 可用，回傳 client 與設定的資料庫.
func ConnectMongo(ctx context.Context, cfg *config.MongoConfig) (*mongo.Client, *mongo.Database, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(cfg.URI))
	if err != nil {
		return nil, nil, fmt.Errorf("mongo connect: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.WithoutCancel(ctx))
		return nil, nil, fmt.Errorf("mongo ping: %w", err)
	}

	db := client.Database(cfg.Name)
	slog.Info("MongoDB connected", "database", cfg.Name)
	return client, db, nil
}

// CloseMongo 中斷 MongoDB 連線（最多等 5 秒；ctx 已取消也照樣關閉）.
func CloseMongo(ctx context.Context, client *mongo.Client) {
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := client.Disconnect(ctx); err != nil {
		slog.Error("mongo disconnect", "error", err)
	}
}
