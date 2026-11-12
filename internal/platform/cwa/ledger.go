package cwa

import (
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/redact"
	"context"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
)

const (
	windowHours   = 24
	windowCells   = windowHours + 1 // 與過去 24 小時重疊的整點格.
	usageKeyFmt   = "cwa:usage:"
	usageTTL      = 26 * time.Hour
	redisOpBudget = 500 * time.Millisecond
	pauseMax      = 24 * time.Hour
	retrySuffix   = ":retry"
)

// Ledger 用量記帳與額度判斷（F30-33、F30-34、F30-35）.
// 判斷、記帳與暫停狀態都在同一把鎖內（規格 T19 原子性）；警報端與預報端共用同一份.
type Ledger struct {
	rdb          *redis.Client
	s            config.CWASettings
	loc          *time.Location
	now          func() time.Time
	redisTimeout time.Duration
	schedules    map[string]cron.Schedule // stream → 排程；off / 無效 / 無時區 → 沒有鍵.
	forecastN    int

	mu              sync.Mutex
	mem             map[int64]map[string]int
	paused          bool
	pauseUntil      time.Time
	warnedUsageHour int64
	redisWarnHour   int64
	skipLogHour     map[string]int64
}

// NewLedger 建立 Ledger；rdb 為 nil 時只用記憶體. loc 為 nil（時區載入失敗）→ planned 一律為 0.
// 啟動時若目前設定每日預定呼叫數大於 cap 會 Warn 一行（規格 T23）.
// s 不可為 nil，建構時複製一份值（之後改呼叫端的 s 不影響 Ledger）.
func NewLedger(rdb *redis.Client, s *config.CWASettings, loc *time.Location, now func() time.Time) *Ledger {
	if now == nil {
		now = time.Now
	}
	l := &Ledger{
		rdb: rdb, s: *s, loc: loc, now: now, redisTimeout: redisOpBudget,
		schedules: map[string]cron.Schedule{}, forecastN: ForecastDatasetCount,
		mem: map[int64]map[string]int{}, warnedUsageHour: -1, redisWarnHour: -1,
		skipLogHour: map[string]int64{},
	}
	if loc != nil {
		for stream, spec := range map[string]string{
			StreamWarning: s.WarningCron, StreamEarthquake: s.EarthquakeCron,
			StreamTsunami: s.TsunamiCron, StreamForecast: s.ForecastCron,
		} {
			if spec == config.CronOff {
				continue
			}
			if sched, err := config.ParseCWACron(spec); err == nil {
				l.schedules[stream] = sched
			}
		}
	}
	if planned, quota := l.planned(now())[windowHours], l.cap(); planned > quota {
		slog.Warn("cwa configured call rate exceeds quota cap", "planned", planned, "cap", quota)
	}
	return l
}

// cap = floor(DailyLimit × SafetyRatio).
func (l *Ledger) cap() int { return floorRatio(l.s.DailyLimit, l.s.SafetyRatio) }

func floorRatio(n int, ratio float64) int { return int(math.Floor(float64(n)*ratio + 1e-9)) }

// snapshot 與過去 24 小時重疊的 25 個整點格（hour → 欄位 → 次數）.
type snapshot map[int64]map[string]int

// total 五個 stream 欄位之和（:retry 是子集不重複計）；Usage.Total、Gate 的 used_0、警示都用它.
func (sn snapshot) total() int { return sn.usedSince(math.MinInt64) }

// usedSince 小時格 >= from 的 stream 欄位之和.
func (sn snapshot) usedSince(from int64) int {
	n := 0
	for h, cell := range sn {
		if h < from {
			continue
		}
		for _, st := range Streams() {
			n += cell[st]
		}
	}
	return n
}

// field 全視窗內某欄位（例如 "earthquake:retry"）之和.
func (sn snapshot) field(name string) int {
	n := 0
	for _, cell := range sn {
		n += cell[name]
	}
	return n
}

func usageKey(hour int64) string { return usageKeyFmt + strconv.FormatInt(hour, 10) }

// snapshotLocked 讀 Redis（pipeline，500 毫秒）加記憶體. Redis 失敗只用記憶體並把 *redisOK 設為 false.
func (l *Ledger) snapshotLocked(ctx context.Context, h0 int64, redisOK *bool) snapshot {
	sn := snapshot{}
	for h := h0 - windowHours; h <= h0; h++ {
		cell := map[string]int{}
		for f, n := range l.mem[h] {
			cell[f] += n
		}
		sn[h] = cell
	}
	if l.rdb == nil || !*redisOK {
		return sn
	}
	cmds := make(map[int64]*redis.MapStringStringCmd, windowCells)
	err := l.redisOp(ctx, func(rctx context.Context) error {
		_, perr := l.rdb.Pipelined(rctx, func(p redis.Pipeliner) error {
			for h := h0 - windowHours; h <= h0; h++ {
				cmds[h] = p.HGetAll(rctx, usageKey(h))
			}
			return nil
		})
		return perr
	})
	if err != nil {
		*redisOK = false
		l.warnRedisLocked(err, h0)
		return sn
	}
	for h, cmd := range cmds {
		for f, v := range cmd.Val() {
			if n, convErr := strconv.Atoi(v); convErr == nil {
				sn[h][f] += n
			}
		}
	}
	return sn
}

// recordLocked 記一筆（發出請求之前）. 先寫 Redis；失敗才寫記憶體，同一筆不會兩邊都算.
func (l *Ledger) recordLocked(ctx context.Context, spec CallSpec, attempt int, h0 int64, redisOK *bool) {
	fields := []string{spec.Stream}
	if attempt > 1 {
		fields = append(fields, spec.Stream+retrySuffix)
	}
	if l.rdb != nil && *redisOK {
		key := usageKey(h0)
		err := l.redisOp(ctx, func(rctx context.Context) error {
			_, perr := l.rdb.Pipelined(rctx, func(p redis.Pipeliner) error {
				for _, f := range fields {
					p.HIncrBy(rctx, key, f, 1)
				}
				p.Expire(rctx, key, usageTTL) // 每次寫入都重設 TTL（冪等）.
				return nil
			})
			return perr
		})
		if err == nil {
			return
		}
		*redisOK = false
		l.warnRedisLocked(err, h0)
	}
	cell := l.mem[h0]
	if cell == nil {
		cell = map[string]int{}
		l.mem[h0] = cell
	}
	for _, f := range fields {
		cell[f]++
	}
	for h := range l.mem { // 記憶體只留 25 格.
		if h < h0-windowHours {
			delete(l.mem, h)
		}
	}
}

// redisOp 以 redisTimeout 限制單次 Redis 操作. go-redis 預設不看 ctx 的期限（ContextTimeoutEnabled 關閉），
// 所以在背景 goroutine 執行、逾時就放棄等待；放棄後的結果不再被讀取（極端情況下該筆之後才寫進 Redis，已知限制）.
func (l *Ledger) redisOp(ctx context.Context, fn func(ctx context.Context) error) error {
	rctx, cancel := context.WithTimeout(ctx, l.redisTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn(rctx) }()
	select {
	case err := <-done:
		return err
	case <-rctx.Done():
		return rctx.Err()
	}
}

// warnRedisLocked Redis 失敗的 Warn，每小時至多一行；錯誤文字經 redact.
func (l *Ledger) warnRedisLocked(err error, h0 int64) {
	if l.redisWarnHour == h0 {
		return
	}
	l.redisWarnHour = h0
	slog.Warn("cwa usage ledger redis failed, falling back to memory", "error", redact.Error(err))
}

// Usage 過去 24 小時用量（唯讀；不寫入、不觸發額度判斷）.
func (l *Ledger) Usage(ctx context.Context) Usage {
	l.mu.Lock()
	defer l.mu.Unlock()
	h0 := l.now().Unix() / 3600
	redisOK := true
	sn := l.snapshotLocked(ctx, h0, &redisOK)
	by := make(map[string]int, len(Streams()))
	for _, st := range Streams() {
		by[st] = sn.field(st)
	}
	return Usage{Total: sn.total(), ByStream: by, Limit: l.s.DailyLimit, WindowHours: windowHours}
}

// tier 呼叫優先序：1 = 地震、海嘯；2 = 特報；3 = 低溫、預報（與未知 stream）.
func tier(stream string) int {
	switch stream {
	case StreamEarthquake, StreamTsunami:
		return 1
	case StreamWarning:
		return 2
	default:
		return 3
	}
}

// reserveFor 第 t 級呼叫要替更高優先級預留的量 = 更高級各 stream 在視窗內的重試次數.
func (sn snapshot) reserveFor(t int) int {
	n := 0
	if t >= 2 {
		n += sn.field(StreamEarthquake+retrySuffix) + sn.field(StreamTsunami+retrySuffix)
	}
	if t >= 3 {
		n += sn.field(StreamWarning + retrySuffix)
	}
	return n
}

// reserve 判斷 + 記帳（同一把鎖，F30-34 原子性）. 第 1 次嘗試且 Exempt → 只記帳；
// 其餘通過判斷才記一筆，否則回 ErrQuotaReserved（不記帳、不發請求）.
func (l *Ledger) reserve(ctx context.Context, spec CallSpec, attempt int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	h0 := now.Unix() / 3600
	redisOK := l.rdb != nil
	l.expirePauseLocked(now)
	sn := l.snapshotLocked(ctx, h0, &redisOK)
	if attempt != 1 || !spec.Exempt {
		if reason := l.denyLocked(now, h0, sn, spec); reason != "" {
			l.logSkippedLocked(spec.Stream, reason, h0, sn.total())
			return ErrQuotaReserved
		}
	}
	l.recordLocked(ctx, spec, attempt, h0, &redisOK)
	l.warnUsageLocked(sn.total()+1, h0)
	return nil
}

// denyLocked 回拒絕理由（"paused" | "quota_reserved"），通過回空字串.
// 對 k = 0..24：used_k + planned_k + reserve + 1 ≤ cap 才放行.
func (l *Ledger) denyLocked(now time.Time, h0 int64, sn snapshot, spec CallSpec) string {
	if l.paused {
		return "paused"
	}
	planned := l.planned(now)
	reserve := sn.reserveFor(tier(spec.Stream))
	quota := l.cap()
	for k := 0; k <= windowHours; k++ {
		used := sn.usedSince(h0 + int64(k) - windowHours)
		if used+planned[k]+reserve+1 > quota {
			return "quota_reserved"
		}
	}
	return ""
}

func (l *Ledger) logSkippedLocked(stream, reason string, h0 int64, used int) {
	if last, ok := l.skipLogHour[stream]; ok && last == h0 {
		return
	}
	l.skipLogHour[stream] = h0
	slog.Info("cwa fetch skipped", "stream", stream, "reason", reason, "used", used, "cap", l.cap())
}

func (l *Ledger) warnUsageLocked(total int, h0 int64) {
	warnAt := floorRatio(l.s.DailyLimit, l.s.WarnRatio)
	if warnAt <= 0 || total < warnAt || l.warnedUsageHour == h0 {
		return
	}
	l.warnedUsageHour = h0
	slog.Warn("cwa quota usage high", "used", total, "limit", l.s.DailyLimit)
}

// planned 回傳 k = 0..24 小時內「依目前設定預定會發生」的呼叫數（累計）. 不得寫死任何數字.
// 低溫以特報排程的觸發時刻模擬（規格 B3）：f ≥ nextCold 才計一次並令 nextCold = f + 間隔；特報 off → 低溫 0.
func (l *Ledger) planned(now time.Time) [windowCells]int {
	var delta [windowCells]int
	bucket := func(t time.Time) int {
		k := int((t.Sub(now) + time.Hour - 1) / time.Hour)
		return min(max(k, 0), windowHours)
	}
	horizon := now.Add(windowHours * time.Hour)
	triggers := func(stream string) []time.Time {
		sched, ok := l.schedules[stream]
		if !ok {
			return nil
		}
		var out []time.Time
		for t := sched.Next(now); !t.IsZero() && !t.After(horizon); t = sched.Next(t) {
			out = append(out, t)
		}
		return out
	}
	for _, st := range []string{StreamWarning, StreamEarthquake, StreamTsunami} {
		for _, t := range triggers(st) {
			delta[bucket(t)]++
		}
	}
	for _, t := range triggers(StreamForecast) {
		delta[bucket(t)] += l.forecastN
	}
	nextCold := now
	for _, f := range triggers(StreamWarning) {
		if !f.Before(nextCold) {
			delta[bucket(f)]++
			nextCold = f.Add(l.s.ColdInterval)
		}
	}
	var out [windowCells]int
	sum := 0
	for k := range out {
		sum += delta[k]
		out[k] = sum
	}
	return out
}

// expirePauseLocked 暫停最長 24 小時自動解除.
func (l *Ledger) expirePauseLocked(now time.Time) {
	if l.paused && !now.Before(l.pauseUntil) {
		l.paused = false
		slog.Info("cwa quota pause expired, low-priority calls resumed")
	}
}

// noteResponse 依回應狀態碼更新暫停狀態（F30-35）：清單內狀態碼 → 進入 / 重設暫停；
// 只有 Exempt 第一次嘗試回 2xx 才解除；其他回應不改變狀態.
func (l *Ledger) noteResponse(spec CallSpec, attempt, status int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.expirePauseLocked(now)
	if slices.Contains(l.s.ExhaustedStatus, status) {
		if !l.paused {
			slog.Warn("cwa quota exhausted response, pausing low-priority calls", "status", status)
		}
		l.paused = true
		l.pauseUntil = now.Add(pauseMax)
		return
	}
	if l.paused && spec.Exempt && attempt == 1 && status >= 200 && status < 300 {
		l.paused = false
		slog.Info("cwa quota recovered, low-priority calls resumed")
	}
}
