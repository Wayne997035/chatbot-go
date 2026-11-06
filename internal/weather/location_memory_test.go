package weather

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	weatherstore "chatbot-go/internal/storage/database/weather"

	"github.com/redis/go-redis/v9"
)

// locFakeRedis 只實作 GET / SET / DEL 的假 Redis（go-redis Hook）；SET 的 []byte 值要存成 string.
type locFakeRedis struct{ data map[string]string }

func (f *locFakeRedis) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) { return nil, fmt.Errorf("dial blocked") }
}

func (f *locFakeRedis) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		switch c := cmd.(type) {
		case *redis.StatusCmd:
			key, _ := args[1].(string)
			switch v := args[2].(type) {
			case []byte:
				f.data[key] = string(v)
			default:
				f.data[key] = fmt.Sprint(v)
			}
			c.SetVal("OK")
			return nil
		case *redis.StringCmd:
			key, _ := args[1].(string)
			v, ok := f.data[key]
			if !ok {
				c.SetErr(redis.Nil)
				return redis.Nil
			}
			c.SetVal(v)
			return nil
		case *redis.IntCmd:
			var n int64
			for _, a := range args[1:] {
				key, _ := a.(string)
				if _, ok := f.data[key]; ok {
					delete(f.data, key)
					n++
				}
			}
			c.SetVal(n)
			return nil
		}
		return fmt.Errorf("unsupported command %v", args[0])
	}
}

func (f *locFakeRedis) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// locFakeRepo 對任何城市 / 區域都回一筆預報.
type locFakeRepo struct{ weatherstore.WeatherRepository }

func (locFakeRepo) FindByDistrictAndCity(_ context.Context, district, city string) (*weatherstore.WeatherForecast, error) {
	return &weatherstore.WeatherForecast{City: city, District: district}, nil
}

func newLocTestUserLocations(t *testing.T, data map[string]string) (*UserLocations, *locFakeRedis) {
	t.Helper()
	fake := &locFakeRedis{data: data}
	rc := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	rc.AddHook(fake)
	u := NewUserLocations(rc, NewLookup(locFakeRepo{}, nil, 60))
	if u == nil {
		t.Fatal("NewUserLocations returned nil")
	}
	return u, fake
}

// TestUserLocationsIgnoresOldKeys [F27-01] 舊前綴（loc:）下 bug 期間寫錯的地點不再被讀到.
func TestUserLocationsIgnoresOldKeys(t *testing.T) {
	ctx := context.Background()
	u, fake := newLocTestUserLocations(t, map[string]string{"loc:U1": "新北市|三峽區"})

	if legacyLocationKeyPrefix != "loc:" {
		t.Fatalf("legacyLocationKeyPrefix = %q, want %q", legacyLocationKeyPrefix, "loc:")
	}
	if strings.HasPrefix(locationKeyPrefix, legacyLocationKeyPrefix) {
		t.Errorf("locationKeyPrefix %q falls inside the old loc: key space; old builds could write keys the new build reads", locationKeyPrefix)
	}

	if got := u.ForUser(ctx, "U1"); got != "" {
		t.Errorf("ForUser read the old key, got %q, want empty", got)
	}

	// 正對照：新前綴照常記住、讀出.
	u.Remember(ctx, "U1", "台北市", "信義區")
	if got := fake.data[locationKeyPrefix+"U1"]; got != "台北市|信義區" {
		t.Errorf("stored value under %q = %q, want %q", locationKeyPrefix+"U1", got, "台北市|信義區")
	}
	got := u.ForUser(ctx, "U1")
	if !strings.Contains(got, "台北市信義區") {
		t.Errorf("ForUser = %q, want it to contain 台北市信義區", got)
	}
	if strings.Contains(got, "三峽") {
		t.Errorf("ForUser = %q, must not contain 三峽", got)
	}
}

// TestUserLocationsForgetDeletesOldAndNewKeys [F27-02] 刪除時新舊前綴的 key 都刪，且只刪本人.
func TestUserLocationsForgetDeletesOldAndNewKeys(t *testing.T) {
	u, fake := newLocTestUserLocations(t, map[string]string{
		"loc:U1":  "新北市|三峽區",
		"loc2:U1": "台北市|信義區",
		"loc2:U2": "台中市|西屯區",
	})

	if err := u.Forget(context.Background(), "U1"); err != nil {
		t.Fatalf("Forget returned error: %v", err)
	}
	for _, k := range []string{"loc:U1", "loc2:U1"} {
		if v, ok := fake.data[k]; ok {
			t.Errorf("key %q still present after Forget (value %q)", k, v)
		}
	}
	if v := fake.data["loc2:U2"]; v != "台中市|西屯區" {
		t.Errorf("other user's key loc2:U2 = %q, want it untouched", v)
	}
}
