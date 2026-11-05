package weather

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	weatherstore "chatbot-go/internal/storage/database/weather"

	"github.com/redis/go-redis/v9"
)

// locationFlakyHook go-redis Hook：down 為 true 時指令回 error，否則不呼叫 next 直接當作成功（不連網路）.
type locationFlakyHook struct{ down atomic.Bool }

func (h *locationFlakyHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *locationFlakyHook) ProcessHook(_ redis.ProcessHook) redis.ProcessHook {
	return func(_ context.Context, cmd redis.Cmder) error {
		if h.down.Load() {
			err := errors.New("redis down")
			cmd.SetErr(err)
			return err
		}
		return nil
	}
}

func (h *locationFlakyHook) ProcessPipelineHook(_ redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(_ context.Context, _ []redis.Cmder) error {
		if h.down.Load() {
			return errors.New("redis down")
		}
		return nil
	}
}

// [F24-71] Redis 壞 -> 好 -> 壞：好的那次寫入成功後本地舊地點要清掉，第二次壞時不得讀回舊值.
func TestUserLocationsClearsLocalAfterRedisRecovers(t *testing.T) {
	ctx := context.Background()
	hook := &locationFlakyHook{}
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { _ = c.Close() })
	c.AddHook(hook)
	u := NewUserLocations(c, locationLookup())

	hook.down.Store(true)
	u.Remember(ctx, "U1", "台北市", "信義區") // 壞：寫進本地
	if val, ok := u.recall(ctx, "U1"); !ok || val != "台北市|信義區" {
		t.Fatalf("local fallback should hold old, got %q %v", val, ok)
	}
	hook.down.Store(false)
	u.Remember(ctx, "U1", "高雄市", "左營區") // 好：Redis 寫入成功
	hook.down.Store(true)
	if val, ok := u.recall(ctx, "U1"); ok {
		t.Errorf("stale local value must not be read back after Redis recovered, got %q", val)
	}
}

func locationLookup() *Lookup {
	repo := &countingRepo{cityRow: &weatherstore.WeatherForecast{
		City: "台北市", District: "信義區",
		Elements: []weatherstore.ForecastElement{{ElementName: "T", Description: "溫度", Value: "25"}},
	}}
	return NewLookup(repo, nil, 60)
}

// [F24-43] Redis 為 nil 時記住地點仍可用.
func TestUserLocationsWorksWithoutRedis(t *testing.T) {
	ctx := context.Background()

	if NewUserLocations(nil, nil) != nil {
		t.Error("NewUserLocations(nil, nil) should stay nil")
	}
	u := NewUserLocations(nil, locationLookup())
	if u == nil {
		t.Fatal("NewUserLocations(nil, lookup) should be non-nil")
	}

	tests := []struct {
		name   string
		userID string
		remem  string
		want   string // 空字串 = 預期回 ""
	}{
		{"remembered user", "U1", "U1", "台北市信義區"},
		{"other user empty", "U2", "U1", ""},
		{"empty userID not stored", "", "", ""},
	}
	u.Remember(ctx, "U1", "台北市", "信義區")
	u.Remember(ctx, "", "台北市", "信義區")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := u.ForUser(ctx, tt.userID)
			if tt.want == "" && got != "" {
				t.Errorf("got %q, want empty", got)
			}
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Errorf("got %q, want contains %q", got, tt.want)
			}
		})
	}
}

// [F24-43] Redis 出錯時走本地；Forget 回 Redis err 但本地已清.
func TestUserLocationsForgetClearsLocalEvenWhenRedisFails(t *testing.T) {
	ctx := context.Background()
	u := NewUserLocations(deadRedis(t), locationLookup())
	u.Remember(ctx, "U1", "台北市", "信義區")
	if got := u.ForUser(ctx, "U1"); !strings.Contains(got, "台北市信義區") {
		t.Fatalf("local fallback missing: %q", got)
	}
	if err := u.Forget(ctx, "U1"); err == nil {
		t.Error("Forget should return the Redis error")
	}
	if got := u.ForUser(ctx, "U1"); got != "" {
		t.Errorf("local not cleared: %q", got)
	}
	if err := u.Forget(ctx, "U9"); err == nil {
		t.Error("Forget of unknown user should still surface the Redis error")
	}
}
