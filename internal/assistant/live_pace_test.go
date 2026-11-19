//go:build liveeval

package assistant

import (
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// livePacer 真模型重播的節流：任兩次模型呼叫的開始時間至少隔 livePaceGap（預設 4 秒，LIVE_PACE_SECONDS 可調，0 = 不節流）.
// 免費方案有每分鐘上限（NVIDIA 論壇多數回報每分鐘 40 次，未見官方文件；Groq、Gemini 另有每分鐘與每日上限），
// 不節流的一輪重播幾分鐘內打出上百次，429 與逾時把結果洗掉，分不出是邏輯還是供應商.
var livePacer struct {
	sync.Mutex
	last time.Time
}

func livePaceGap() time.Duration {
	if s := os.Getenv("LIVE_PACE_SECONDS"); s != "" {
		if n, err := strconv.ParseFloat(s, 64); err == nil && n >= 0 {
			return time.Duration(n * float64(time.Second))
		}
	}
	return 4 * time.Second
}

// livePace 在呼叫 Answer / AnswerWith / PlanEdit 之前叫：等待放在呼叫外面，才不會吃掉問答 40 秒、規劃器 10 秒的時限.
func livePace() {
	livePacer.Lock()
	defer livePacer.Unlock()
	if wait := time.Until(livePacer.last.Add(livePaceGap())); wait > 0 {
		time.Sleep(wait)
	}
	livePacer.last = time.Now()
}

// TestLivePaceSpacesCalls 不呼叫模型、不需要金鑰：連續兩次 livePace 至少隔設定的秒數（節流是空殼時這支會紅）.
func TestLivePaceSpacesCalls(t *testing.T) {
	t.Setenv("LIVE_PACE_SECONDS", "0.3")
	livePace()
	start := time.Now()
	livePace()
	if got := time.Since(start); got < 250*time.Millisecond {
		t.Errorf("second livePace returned after %v, want about 300ms", got)
	}
}
