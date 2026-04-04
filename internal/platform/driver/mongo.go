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

var (
	mongoClient   *mongo.Client
	mongoDatabase *mongo.Database
)

func ConnectMongo(cfg *config.Config) error {
	clientOpts := options.Client().ApplyURI(cfg.Database.Mongo.URI)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(clientOpts)
	if err != nil {
		return fmt.Errorf("mongo connect: %w", err)
	}

	if err := client.Ping(ctx, nil); err != nil {
		return fmt.Errorf("mongo ping: %w", err)
	}

	mongoClient = client
	mongoDatabase = client.Database(cfg.Database.Mongo.Name)
	slog.Info("MongoDB connected", "database", cfg.Database.Mongo.Name)
	return nil
}

func GetMongoDatabase() *mongo.Database {
	return mongoDatabase
}

func CloseMongo() {
	if mongoClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := mongoClient.Disconnect(ctx); err != nil {
			slog.Error("mongo disconnect", "error", err)
		}
	}
}
