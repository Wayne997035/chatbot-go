package driver

import (
	"chatbot-go/internal/platform/config"
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

var redisClient *redis.Client

func ConnectRedis(cfg *config.Config) error {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Database.Redis.Addr,
		Password: cfg.Database.Redis.Password,
		DB:       cfg.Database.Redis.DB,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}

	redisClient = client
	slog.Info("Redis connected", "addr", cfg.Database.Redis.Addr)
	return nil
}

func GetRedisClient() *redis.Client {
	return redisClient
}

func CloseRedis() {
	if redisClient != nil {
		if err := redisClient.Close(); err != nil {
			slog.Error("redis close", "error", err)
		}
	}
}
