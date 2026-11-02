package driver

import (
	"chatbot-go/internal/platform/config"
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// ConnectRedis 連線並確認 Redis 可用.
func ConnectRedis(ctx context.Context, cfg *config.RedisConfig) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	slog.Info("Redis connected", "addr", cfg.Addr)
	return client, nil
}

// CloseRedis 關閉 Redis 連線.
func CloseRedis(client *redis.Client) {
	if client == nil {
		return
	}
	if err := client.Close(); err != nil {
		slog.Error("redis close", "error", err)
	}
}
