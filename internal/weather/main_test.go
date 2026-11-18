package weather

import (
	"os"
	"testing"
	"time"
)

// TestMain 在任何測試與背景 goroutine 開始前把 time.Local 設成 UTC（容器沒設 TZ 的情況，F30-51 / F30-52 要守的前提）.
// 不在個別測試裡改再還原：time.Local 是全域變數，計時器 goroutine 的 time.Now 會同時讀它，-race 會抓到資料競爭.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}
