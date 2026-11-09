package weather

import (
	"chatbot-go/internal/platform/localcache"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// 記住使用者最近查詢的地點，讓自由問答可以參考當地天氣.
const (
	// [F27-01] 新前綴刻意不以舊前綴 loc: 開頭：若新前綴是 loc:v2: 這種形狀，部署期間仍在跑的舊版
	// 收到字面為 v2:<id> 的 userID 就會寫進新版讀得到的 key；loc2: 與 loc: 互不為前綴，不會發生。
	locationKeyPrefix = "loc2:"
	// legacyLocationKeyPrefix 舊前綴。bug 期間舊前綴下寫入了錯誤地點（例：「安安」被比對成三峽區），
	// 不再讀取；只在 Forget 時一併刪除，其餘留到 TTL 自然消失.
	legacyLocationKeyPrefix = "loc:"
	locationTTL             = 30 * 24 * time.Hour

	// [F24-43] 本地快取容量；TTL 長，是 7 處中最易滿的一處.
	locationLocalMaxSize = 5000
)

// UserLocations 使用者最近查詢的地點.
type UserLocations struct {
	redis  *redis.Client
	lookup *Lookup
	local  *localcache.Cache[string]
}

// NewUserLocations 建立；lookup 為 nil 時回傳 nil（不記地點）.
// [F24-43] redis 為 nil 不再回 nil，改用本地快取.
func NewUserLocations(redisClient *redis.Client, lookup *Lookup) *UserLocations {
	if lookup == nil {
		return nil
	}
	return &UserLocations{
		redis:  redisClient,
		lookup: lookup,
		local:  localcache.New[string]("weather.userloc", locationLocalMaxSize, locationTTL),
	}
}

// Remember 記住地點.
func (u *UserLocations) Remember(ctx context.Context, userID, city, district string) {
	if u == nil || userID == "" {
		return
	}
	val := city + "|" + district
	if u.redis != nil {
		if err := u.redis.Set(ctx, locationKeyPrefix+userID, val, locationTTL).Err(); err == nil {
			// [F24-71] Redis 已寫入最新值，清掉故障期間留在本地的舊地點，避免 Redis 再故障時讀回過期值.
			u.local.Delete(userID)
			return
		}
	}
	u.local.Set(userID, val)
}

// Forget 刪除記住的地點（新舊前綴都刪）[F27-02].
func (u *UserLocations) Forget(ctx context.Context, userID string) error {
	if u == nil {
		return nil
	}
	u.local.Delete(userID) // 一律先清本地，Redis 失敗時刪除資料流程才不殘留
	if u.redis == nil {
		return nil
	}
	return u.redis.Del(ctx, locationKeyPrefix+userID, legacyLocationKeyPrefix+userID).Err()
}

// ForUser 回傳最近查詢地點的天氣摘要；沒有時回傳空字串.
func (u *UserLocations) ForUser(ctx context.Context, userID string) string {
	if u == nil {
		return ""
	}
	val, ok := u.recall(ctx, userID)
	if !ok {
		return ""
	}
	city, district, ok := strings.Cut(val, "|")
	if !ok {
		return ""
	}
	f, err := u.lookup.ByCityAndDistrict(ctx, city, district)
	if err != nil || f == nil {
		return ""
	}
	var parts []string
	for _, el := range f.Elements {
		if el.Value != "" && el.ElementName != "WeatherDescription" {
			parts = append(parts, fmt.Sprintf("%s %s%s", el.Description, el.Value, unitSuffix(el.ElementName)))
		}
	}
	return fmt.Sprintf("使用者最近查詢的地點（%s%s）天氣：%s", city, district, strings.Join(parts, "、"))
}

// recall [F24-43] redis.Nil 是權威 miss（不查本地）；Redis 為 nil 或其他錯誤才讀本地.
func (u *UserLocations) recall(ctx context.Context, userID string) (string, bool) {
	if u.redis != nil {
		val, err := u.redis.Get(ctx, locationKeyPrefix+userID).Result()
		if err == nil {
			return val, true
		}
		if errors.Is(err, redis.Nil) {
			return "", false
		}
	}
	return u.local.Get(userID)
}
