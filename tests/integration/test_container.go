package integration

import (
	"context"
	"fmt"
	"io"
	"strings"
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

	// 不設 MONGO_INITDB_ROOT_USERNAME/PASSWORD：設了的話 image 會先啟動一個只聽容器內 127.0.0.1 的暫時 mongod
	// 建帳號、關掉再正式啟動，等待條件可能在暫時那台就成立，測試連線時正式的還沒起來（CI 偶發 connection refused）.
	// 測試 container 用完即丟，不需要帳號密碼；mongod 只啟動一次，「ready」就是真的 ready.
	mongodb, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "mongo:8.0",
			ExposedPorts: []string{"27017/tcp"},
			Env: map[string]string{
				// MongoDB 8.0 在 Linux kernel 6.19+（GitHub Actions runner）啟動即退出（SERVER-121912）.
				// 讓 glibc 持有 rseq，tcmalloc 改用 per-thread cache，避開 mongod 的啟動檢查.
				"GLIBC_TUNABLES": "glibc.pthread.rseq=1",
			},
			WaitingFor: wait.ForAll(
				wait.ForLog("Waiting for connections").WithOccurrence(1),
				wait.ForListeningPort("27017/tcp"),
			).WithDeadline(90 * time.Second), // 使用 WithDeadline 取代棄用的 WithStartupTimeout
		},
		Started: true,
	})
	tm := &TestMongoDB{Container: mongodb}
	// 先註冊清理，後面任何一步失敗都不會留下 container.
	t.Cleanup(func() { tm.Cleanup(t) })
	if err != nil {
		t.Fatalf("Failed to start MongoDB container: %v", err)
	}

	host, err := mongodb.Host(ctx)
	if err != nil {
		t.Fatalf("Failed to get MongoDB host: %v", err)
	}
	port, err := mongodb.MappedPort(ctx, "27017/tcp")
	if err != nil {
		t.Fatalf("Failed to get MongoDB port: %v", err)
	}

	client, err := mongo.Connect(options.Client().ApplyURI(fmt.Sprintf("mongodb://%s:%s", host, port.Port())))
	if err != nil {
		t.Fatalf("Failed to connect to MongoDB: %v", err)
	}
	tm.Client = client

	pingCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, nil); err != nil {
		t.Fatalf("Failed to ping MongoDB: %v\ncontainer log tail:\n%s", err, logTail(ctx, mongodb, 40))
	}

	tm.Database = client.Database("chatbot_go_test")
	return tm
}

// logTail 回傳 container 最後 n 行 log；連線失敗時用來判斷 mongod 是還沒起來還是已經退出.
func logTail(ctx context.Context, c testcontainers.Container, n int) string {
	r, err := c.Logs(ctx)
	if err != nil {
		return fmt.Sprintf("(cannot read container logs: %v)", err)
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return fmt.Sprintf("(cannot read container logs: %v)", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
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

	// 停止並刪除 container；TerminateContainer 會處理啟動失敗時拿到的 nil container
	if err := testcontainers.TerminateContainer(tm.Container); err != nil {
		t.Logf("Failed to terminate MongoDB container: %v", err)
	}
}

// GetCollection 獲取指定的 collection.
func (tm *TestMongoDB) GetCollection(name string) *mongo.Collection {
	return tm.Database.Collection(name)
}
