package nutrition

import (
	"chatbot-go/internal/platform/localcache"
	"chatbot-go/internal/redact"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
)

// [F37-41] 記餐文字 AI 估算結果快取（跨使用者共用、值不含使用者資料）.
//
// 兩層：本機 localcache + 可選 Redis（TTL 7 天）. 查詢在 EstimateText 呼叫 Completer 之前，命中就不呼叫 AI、不扣額度.
// 值只含食物、營養、店名（name+source）、餐別、是否點心、建議；NEVER 存 userID、使用者原文、時間戳、DayOffset、Inferred.
// 日誌與計數 NEVER 帶文字、key 或任何文字雜湊.

const (
	estimateTTL       = 7 * 24 * time.Hour
	estimateLocalSize = 2000
	// maxCacheTextRunes 超過的文字不快取（很少重複、也比較可能帶個人細節）.
	maxCacheTextRunes = 80
	// maxCacheValueBytes 編碼後的值上限.
	maxCacheValueBytes = 8 << 10
	// maxCacheFoods 單餐食物數上限.
	maxCacheFoods = 20
	// redisCallTimeout 單一 Redis 呼叫最多等多久（含「接受連線但不回應」）.
	redisCallTimeout = 300 * time.Millisecond
	// redisWarnEvery Redis 失敗 Warn 的最小間隔.
	redisWarnEvery = time.Minute
)

// 快取層名稱（日誌 layer 欄位）.
const (
	layerLocal = "local"
	layerRedis = "redis"
	layerNone  = "none"
)

// 不存的原因（日誌 skip 欄位與 Stats().Skips 的 key）.
const (
	skipAIError   = "ai_error"
	skipNotFood   = "not_food"
	skipClarify   = "clarify"
	skipMultiMeal = "multi_meal"
	skipDayOffset = "day_offset"
	skipTooLong   = "too_long"
	skipOversize  = "oversize"
)

// cachedFood 快取值裡的一樣食物：只有這六欄.
type cachedFood struct {
	Name     string  `json:"name"`
	Portion  string  `json:"portion"`
	Calories float64 `json:"calories"`
	ProteinG float64 `json:"protein_g"`
	CarbsG   float64 `json:"carbs_g"`
	FatG     float64 `json:"fat_g"`
}

// cachedPlace 店名與來源（user / dish，決定回覆措辭）；NEVER 存 address.
type cachedPlace struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

type cachedMeal struct {
	Foods     []cachedFood `json:"foods"`
	Place     *cachedPlace `json:"place"`
	MealType  string       `json:"meal_type"`
	SnackOnly bool         `json:"snack_only"`
	Advice    string       `json:"advice"`
}

// cachedEstimate 快取值；V 是 schema 版本.
type cachedEstimate struct {
	V    int        `json:"v"`
	Meal cachedMeal `json:"meal"`
}

// EstimateCacheStats 程序內計數快照.
type EstimateCacheStats struct {
	HitsLocal int64
	HitsRedis int64
	Misses    int64
	Skips     map[string]int64
}

// EstimateCache 記餐估算快取；nil 的 *EstimateCache 什麼都不做.
type EstimateCache struct {
	redis *redis.Client
	local *localcache.Cache[cachedEstimate]

	hitsLocal atomic.Int64
	hitsRedis atomic.Int64
	misses    atomic.Int64
	skips     map[string]*atomic.Int64
	lastWarn  atomic.Int64
	now       func() time.Time
}

var skipReasons = []string{skipAIError, skipNotFood, skipClarify, skipMultiMeal, skipDayOffset, skipTooLong, skipOversize}

// NewEstimateCache 建立快取；client 為 nil（沒有 Redis）時只用本機層.
// 參數必須是具體的 *redis.Client：nil 指標在函式內以 == nil 判斷，不經 interface.
func NewEstimateCache(client *redis.Client) *EstimateCache {
	c := &EstimateCache{
		redis: client,
		local: localcache.New[cachedEstimate]("mealest", estimateLocalSize, estimateTTL),
		skips: make(map[string]*atomic.Int64, len(skipReasons)),
		now:   time.Now,
	}
	for _, r := range skipReasons {
		c.skips[r] = new(atomic.Int64)
	}
	return c
}

// Stats 目前計數快照（測試與除錯用）.
func (c *EstimateCache) Stats() EstimateCacheStats {
	s := EstimateCacheStats{
		HitsLocal: c.hitsLocal.Load(), HitsRedis: c.hitsRedis.Load(), Misses: c.misses.Load(),
		Skips: make(map[string]int64, len(c.skips)),
	}
	for r, n := range c.skips {
		s.Skips[r] = n.Load()
	}
	return s
}

// lookup 先查本機、再查 Redis；命中回傳深複本與命中層. 任何錯誤、逾時、壞資料都當未命中.
func (c *EstimateCache) lookup(ctx context.Context, key, text string) (*TextEstimate, string) {
	if utf8.RuneCountInString(text) > maxCacheTextRunes {
		return nil, layerNone // 太長的文字一定不會存，不必查
	}
	if v, ok := c.local.Get(key); ok {
		if est := v.toEstimate(); est != nil {
			return est, layerLocal
		}
	}
	if c.redis == nil {
		return nil, layerNone
	}
	raw, err := c.redisDo(ctx, "get", func(cctx context.Context) (string, error) {
		return c.redis.Get(cctx, key).Result()
	})
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			c.warnRedis("get", err)
		}
		return nil, layerNone
	}
	var v cachedEstimate
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		c.warnRedis("decode", err) // 壞資料當未命中，由這次 AI 結果覆寫；Warn 不帶 key 或內容
		return nil, layerNone
	}
	est := v.toEstimate()
	if est == nil {
		return nil, layerNone
	}
	c.local.Set(key, v)
	return est, layerRedis
}

// store 存一筆；不可快取回傳原因（stored=false），否則兩層都寫（Redis 盡力而為）.
func (c *EstimateCache) store(ctx context.Context, key, text string, est *TextEstimate) (stored bool, skip string) {
	if skip = cacheSkipReason(text, est); skip != "" {
		return false, skip
	}
	v := newCachedEstimate(&est.Meals[0])
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > maxCacheValueBytes {
		return false, skipOversize
	}
	c.local.Set(key, v)
	if c.redis != nil {
		if _, err := c.redisDo(ctx, "set", func(cctx context.Context) (string, error) {
			return c.redis.Set(cctx, key, raw, estimateTTL).Result()
		}); err != nil {
			c.warnRedis("set", err)
		}
	}
	return true, ""
}

// cacheSkipReason 回傳不快取的原因；可快取回傳空字串.
func cacheSkipReason(text string, est *TextEstimate) string {
	switch {
	case est == nil || !est.IsFood:
		return skipNotFood
	case est.Clarify != nil:
		return skipClarify
	case len(est.Meals) != 1 || est.Dropped > 0:
		return skipMultiMeal
	case est.Meals[0].DayOffset != 0:
		return skipDayOffset
	case utf8.RuneCountInString(text) > maxCacheTextRunes:
		return skipTooLong
	case len(est.Meals[0].Foods) < 1 || len(est.Meals[0].Foods) > maxCacheFoods:
		return skipOversize
	}
	return ""
}

func newCachedEstimate(m *MealEstimate) cachedEstimate {
	v := cachedEstimate{V: estimateSchema, Meal: cachedMeal{
		Foods:     make([]cachedFood, 0, len(m.Foods)),
		MealType:  string(m.MealType),
		SnackOnly: m.SnackOnly,
		Advice:    m.Advice,
	}}
	for i := range m.Foods {
		f := &m.Foods[i]
		v.Meal.Foods = append(v.Meal.Foods, cachedFood{
			Name: f.Name, Portion: f.Portion, Calories: f.Calories, ProteinG: f.ProteinG, CarbsG: f.CarbsG, FatG: f.FatG,
		})
	}
	if m.Place != nil {
		v.Meal.Place = &cachedPlace{Name: m.Place.Name, Source: m.Place.Source}
	}
	return v
}

// toEstimate 重新驗證並建一份全新的估算（深複本，Total 由食物重算）；不合格回傳 nil（當未命中）.
func (v *cachedEstimate) toEstimate() *TextEstimate {
	m := &v.Meal
	if v.V != estimateSchema || len(m.Foods) < 1 || len(m.Foods) > maxCacheFoods {
		return nil
	}
	if mt := MealType(m.MealType); mt != "" && !mt.Valid() {
		return nil
	}
	out := MealEstimate{
		MealType:  MealType(m.MealType),
		Advice:    m.Advice,
		SnackOnly: m.SnackOnly,
		Foods:     make([]meal.FoodItem, 0, len(m.Foods)),
	}
	for i := range m.Foods {
		f := &m.Foods[i]
		if f.Name == "" || f.Calories < 1 || f.Calories > maxItemCalories {
			return nil
		}
		item := meal.FoodItem{Name: f.Name, Portion: f.Portion, Nutrients: meal.Nutrients{
			Calories: f.Calories, ProteinG: f.ProteinG, CarbsG: f.CarbsG, FatG: f.FatG,
		}}
		out.Foods = append(out.Foods, item)
		out.Total.Add(&item.Nutrients)
	}
	if p := m.Place; p != nil {
		if p.Name == "" || (p.Source != meal.PlaceFromUser && p.Source != meal.PlaceFromDish) {
			return nil
		}
		out.Place = &meal.Place{Name: p.Name, Source: p.Source}
	}
	return &TextEstimate{IsFood: true, Meals: []MealEstimate{out}}
}

// record 計數並寫每次估算一行的 INFO；欄位固定五個，NEVER 帶文字、key 或雜湊.
func (c *EstimateCache) record(allowClarify bool, hitLayer string, stored bool, skip string) {
	result := "miss"
	switch hitLayer {
	case layerLocal:
		c.hitsLocal.Add(1)
		result = "hit"
	case layerRedis:
		c.hitsRedis.Add(1)
		result = "hit"
	default:
		c.misses.Add(1)
		if n, ok := c.skips[skip]; ok {
			n.Add(1)
		}
	}
	layer := hitLayer
	if layer == "" {
		layer = layerNone
	}
	slog.Info("meal estimate cache",
		"result", result, "layer", layer, "stored", stored, "skip", skip, "clarify_mode", allowClarify)
}

var errRedisTimeout = errors.New("redis call timed out")

// redisDo 把一次 Redis 呼叫放進 goroutine，最多等 redisCallTimeout.
// 單靠 context.WithTimeout 擋不住「接受連線但不回應」：production 客戶端沒開 ContextTimeoutEnabled，
// go-redis 不會把 ctx 期限套到 socket 讀寫，且讀取逾時會重試.
func (c *EstimateCache) redisDo(ctx context.Context, _ string, fn func(context.Context) (string, error)) (string, error) {
	type result struct {
		val string
		err error
	}
	cctx, cancel := context.WithTimeout(ctx, redisCallTimeout)
	ch := make(chan result, 1)
	go func() {
		v, err := fn(cctx)
		ch <- result{v, err}
	}()
	timer := time.NewTimer(redisCallTimeout)
	defer func() {
		timer.Stop()
		cancel()
	}()
	select {
	case r := <-ch:
		return r.val, r.err
	case <-timer.C:
		return "", errRedisTimeout
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// warnRedis Redis 失敗的 Warn，每程序每分鐘至多一行；不帶 key 或文字.
func (c *EstimateCache) warnRedis(op string, err error) {
	now := c.now().UnixNano()
	last := c.lastWarn.Load()
	if last != 0 && now-last < int64(redisWarnEvery) {
		return
	}
	if c.lastWarn.CompareAndSwap(last, now) {
		slog.Warn("meal estimate cache redis", "op", op, "error", redact.Error(err))
	}
}
