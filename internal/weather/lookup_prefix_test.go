package weather

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

// F30-55：天氣查詢的 Redis 鍵都帶專屬前綴；使用者輸入 geocode:台北 讀不到別種快取.
func TestLookupRedisKeysArePrefixed(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	const foreignKey = "geocode:台北"
	// 別種快取的內容恰好也是合法的預報 JSON 陣列：沒有前綴時查詢文字 geocode:台北 會把它當天氣結果讀走.
	if err := mr.Set(foreignKey, `[{"id":"x_y","city":"外來","district":"資料"}]`); err != nil {
		t.Fatal(err)
	}
	repo := &countingRepo{
		districtRows: []weatherstore.WeatherForecast{{City: "台北市", District: "信義區", CreateTime: "t1"}},
		cityRow:      &weatherstore.WeatherForecast{City: "台北市", District: "信義區", CreateTime: "t1"},
	}
	l := NewLookup(repo, rdb, 3600)
	ctx := context.Background()

	got, err := l.ByDistrict(ctx, foreignKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		if f.City == "外來" {
			t.Fatalf("lookup of %q read another feature's cache: %+v", foreignKey, got)
		}
	}
	if repo.districtCalls != 1 {
		t.Errorf("districtCalls = %d, the lookup must miss the foreign key and hit the repo", repo.districtCalls)
	}
	if _, err := l.ByDistrict(ctx, "信義區"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ByCityAndDistrict(ctx, "臺北市", "信義區"); err != nil {
		t.Fatal(err)
	}

	var districtKeys, cityKeys int
	for _, k := range mr.Keys() {
		switch {
		case k == foreignKey:
		case strings.HasPrefix(k, "weather:d:"):
			districtKeys++
		case strings.HasPrefix(k, "weather:cd:"):
			cityKeys++
		default:
			t.Errorf("unprefixed Redis key %q", k)
		}
	}
	if districtKeys == 0 || cityKeys == 0 {
		t.Errorf("want both key kinds written, got weather:d: %d, weather:cd: %d (keys %v)", districtKeys, cityKeys, mr.Keys())
	}
}
