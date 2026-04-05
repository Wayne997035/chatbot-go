package integration

import (
	"context"
	"fmt"
	"testing"
    "time"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestMongoDB 提供測試用的 MongoDB container.
// 每個測試都會創建獨立的 container，避免測試間互相影響.
type TestMongoDB struct {
	Container testcontainers.Container
	Client    *mongo.Client
	Database  *mongo.Database
}

// SetupTestMongoDB 設置測試用的 MongoDB container.
// 使用 testcontainers-go 自動管理 container 生命週期.
// container 會使用動態分配的 port，避免與現有服務衝突.
func SetupTestMongoDB(t *testing.T) *TestMongoDB {
	t.Helper()
	ctx := context.Background()

	// 啟動 MongoDB container
	// 使用 mongo:7.0 image，container 內部使用 27017 port
	// 但對外會映射到隨機可用的 port
	mongodb, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "mongo:8.0",
			ExposedPorts: []string{"27017/tcp"},
			Env: map[string]string{
				"MONGO_INITDB_ROOT_USERNAME": "test",
				"MONGO_INITDB_ROOT_PASSWORD": "test",
			},
			WaitingFor: wait.ForAll(
    			wait.ForLog(".*Waiting for connections.*").WithOccurrence(1),
       			wait.ForExposedPort(),
			).WithStartupTimeout(120 * time.Second),  // 加大 timeout
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("Failed to start MongoDB container: %v", err)
	}

	// 獲取 MongoDB 對外映射的 port
	port, err := mongodb.MappedPort(ctx, "27017/tcp")
	if err != nil {
		t.Fatalf("Failed to get MongoDB port: %v", err)
	}

	// 連接到測試 MongoDB
	mongoURI := fmt.Sprintf("mongodb://test:test@localhost:%s", port.Port())
	clientOptions := options.Client().ApplyURI(mongoURI)
	client, err := mongo.Connect(clientOptions)
	if err != nil {
		t.Fatalf("Failed to connect to MongoDB: %v", err)
	}

	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("Failed to ping MongoDB: %v", err)
	}

	database := client.Database("chatbot_go_test")

	tm := &TestMongoDB{
		Container: mongodb,
		Client:    client,
		Database:  database,
	}

	// 使用 t.Cleanup 自動清理，測試結束後自動關閉
	t.Cleanup(func() { tm.Cleanup(t) })

	return tm
}

// Cleanup 清理測試資源.
// 清理順序：1. 刪除測試 DB 2. 斷開 client 連接 3. 停止 container.
func (tm *TestMongoDB) Cleanup(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	// 刪除測試資料庫
	if tm.Database != nil {
		if err := tm.Database.Drop(ctx); err != nil {
			t.Logf("Failed to drop test database: %v", err)
		}
	}

	// 斷開 MongoDB client 連接
	if tm.Client != nil {
		if err := tm.Client.Disconnect(ctx); err != nil {
			t.Logf("Failed to disconnect MongoDB client: %v", err)
		}
	}

	// 停止並刪除 container
	if tm.Container != nil {
		if err := tm.Container.Terminate(ctx); err != nil {
			t.Logf("Failed to terminate MongoDB container: %v", err)
		}
	}
}

// GetCollection 獲取指定的 collection.
func (tm *TestMongoDB) GetCollection(name string) *mongo.Collection {
	return tm.Database.Collection(name)
}
