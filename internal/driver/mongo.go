package driver

import (
	"context"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"chatbot-go/internal/config"
)

// ConnectMongo 建立與 MongoDB 的連線並返回客戶端
func ConnectMongo(config *config.Config) *mongo.Client {

	// 建立客戶端選項
	clientOptions := options.Client().ApplyURI(config.Database.Mongo.URL)

	// 設定連線超時時間
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 連接到 MongoDB
	client, err := mongo.Connect(clientOptions)
	if err != nil {
		log.Fatalf("無法連接到 MongoDB: %v", err)
	}

	// 驗證連線
	if err = client.Ping(ctx, nil); err != nil {
		log.Fatalf("無法 ping MongoDB: %v", err)
	}

	log.Println("成功連接到 MongoDB!")
	return client
}

func NewUsersCollection(client *mongo.Client) *mongo.Collection {
	return client.Database("chatbot").Collection("users")
}
