package weather

import (
	"context"
	"strings"
	"testing"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

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
