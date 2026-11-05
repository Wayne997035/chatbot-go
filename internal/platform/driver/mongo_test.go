package driver

import (
	"chatbot-go/internal/platform/config"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// [SEC-R3-02] 設了 MONGODB_LOG_* 也不讓 mongo-driver 繞過 slog 直寫 stderr.
// driver 在建立 logger 當下才取 os.Stderr，所以先換成管線再連線.
func TestConnectMongoDoesNotWriteStderrUnderLogEnv(t *testing.T) {
	t.Setenv("MONGODB_LOG_ALL", "debug")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	restored := false
	restore := func() {
		if !restored {
			os.Stderr = orig
			restored = true
		}
	}
	t.Cleanup(restore)
	got := make(chan int, 1)
	go func() {
		b, _ := io.ReadAll(r)
		got <- len(b)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := &config.MongoConfig{URI: "mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=300&connectTimeoutMS=200", Name: "x"}
	client, _, err := ConnectMongo(ctx, cfg)
	if err == nil {
		_ = client.Disconnect(ctx)
		t.Fatal("ConnectMongo to an unreachable server should fail")
	}
	restore()
	_ = w.Close()
	select {
	case n := <-got:
		if n != 0 {
			t.Errorf("mongo-driver wrote %d bytes straight to stderr", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stderr pipe was not drained")
	}
}

// [SEC-R3-02] 五個 log 元件都明確關閉. 連不上伺服器時 driver 不會產生 command 元件的 log，
// 上一支測試守不到它，而 command 元件正是會帶文件內容的那一個.
func TestClientOptionsTurnsOffEveryMongoLogComponent(t *testing.T) {
	lo := clientOptions("mongodb://127.0.0.1:1").LoggerOptions
	if lo == nil {
		t.Fatal("no logger options set")
	}
	for _, c := range []options.LogComponent{
		options.LogComponentAll,
		options.LogComponentCommand,
		options.LogComponentTopology,
		options.LogComponentServerSelection,
		options.LogComponentConnection,
	} {
		if lvl, ok := lo.ComponentLevels[c]; !ok || lvl != options.LogLevel(0) {
			t.Errorf("component %v: level %v (set=%v), want off", c, lvl, ok)
		}
	}
}
