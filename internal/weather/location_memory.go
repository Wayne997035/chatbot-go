package weather

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// 記住使用者最近查詢的地點，讓自由問答可以參考當地天氣.
const (
	// [F27-01] 新前綴刻意不以舊前綴 loc: 開頭：舊版仍在跑時，字面為 "2:<id>" 之類的 userID
	// 不能寫進新版讀得到的 key。
	locationKeyPrefix = "loc2:"
	// legacyLocationKeyPrefix 舊前綴。bug 期間舊前綴下寫入了錯誤地點（例：「安安」被比對成三峽區），
	// 不再讀取；只在 Forget 時一併刪除，其餘留到 TTL 自然消失.
	legacyLocationKeyPrefix = "loc:"
	locationTTL             = 30 * 24 * time.Hour
)

// UserLocations 使用者最近查詢的地點.
type UserLocations struct {
	redis  *redis.Client
	lookup *Lookup
}

// NewUserLocations 建立；redis 為 nil 時回傳 nil（不記地點）.
func NewUserLocations(redisClient *redis.Client, lookup *Lookup) *UserLocations {
	if redisClient == nil || lookup == nil {
		return nil
	}
	return &UserLocations{redis: redisClient, lookup: lookup}
}

// Remember 記住地點.
func (u *UserLocations) Remember(ctx context.Context, userID, city, district string) {
	if u == nil || userID == "" {
		return
	}
	u.redis.Set(ctx, locationKeyPrefix+userID, city+"|"+district, locationTTL)
}

// Forget 刪除記住的地點（新舊前綴都刪）[F27-02].
func (u *UserLocations) Forget(ctx context.Context, userID string) error {
	if u == nil {
		return nil
	}
	return u.redis.Del(ctx, locationKeyPrefix+userID, legacyLocationKeyPrefix+userID).Err()
}

// ForUser 回傳最近查詢地點的天氣摘要；沒有時回傳空字串.
func (u *UserLocations) ForUser(ctx context.Context, userID string) string {
	if u == nil {
		return ""
	}
	val, err := u.redis.Get(ctx, locationKeyPrefix+userID).Result()
	if err != nil {
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
