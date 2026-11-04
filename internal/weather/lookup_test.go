package weather

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

var errLookupRepoDown = errors.New("repo down")

// countingRepo 計數 Mongo 查詢次數的假 repo.
type countingRepo struct {
	districtCalls int
	cityCalls     int
	districtRows  []weatherstore.WeatherForecast
	cityRow       *weatherstore.WeatherForecast
	err           error
	lastCity      string
}

func (r *countingRepo) FindByDistrict(context.Context, string) ([]weatherstore.WeatherForecast, error) {
	r.districtCalls++
	if r.err != nil {
		return nil, r.err
	}
	return append([]weatherstore.WeatherForecast(nil), r.districtRows...), nil
}

func (r *countingRepo) FindByDistrictAndCity(_ context.Context, _, city string) (*weatherstore.WeatherForecast, error) {
	r.cityCalls++
	r.lastCity = city
	if r.err != nil {
		return nil, r.err
	}
	if r.cityRow == nil {
		return nil, nil
	}
	cp := *r.cityRow
	return &cp, nil
}

func (r *countingRepo) FindAllDistricts(context.Context) ([]weatherstore.DistrictEntry, error) {
	return nil, errors.New("unexpected")
}

func (r *countingRepo) Upsert(context.Context, *weatherstore.WeatherForecast) error {
	return errors.New("unexpected")
}

// deadRedis 連到一定連不上的埠，所有操作回非 redis.Nil 的 error（repo 沒有 miniredis）.
func deadRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{
		Addr:          "127.0.0.1:1",
		MaxRetries:    -1,
		DialTimeout:   50 * time.Millisecond,
		DialerRetries: 1,
	})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func districtRows() []weatherstore.WeatherForecast {
	return []weatherstore.WeatherForecast{{City: "台北市", District: "信義區", CreateTime: "t1"}}
}

// [F24-41] Redis 為 nil 時走本地快取.
func TestLookupLocalCacheWhenRedisNil(t *testing.T) {
	tests := []struct {
		name      string
		rows      []weatherstore.WeatherForecast
		wantCalls int
	}{
		{"non-empty cached", districtRows(), 1},
		{"empty result not cached", nil, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &countingRepo{districtRows: tt.rows}
			l := NewLookup(repo, nil, 60)
			var first []weatherstore.WeatherForecast
			for i := 0; i < 2; i++ {
				got, err := l.ByDistrict(context.Background(), "信義區")
				if err != nil {
					t.Fatalf("call %d: %v", i, err)
				}
				if i == 0 {
					first = got
				} else if len(got) != len(first) {
					t.Errorf("results differ: %v vs %v", got, first)
				}
			}
			if repo.districtCalls != tt.wantCalls {
				t.Errorf("repo calls = %d, want %d", repo.districtCalls, tt.wantCalls)
			}
		})
	}

	t.Run("caller mutation does not leak into cache", func(t *testing.T) {
		repo := &countingRepo{districtRows: districtRows()}
		l := NewLookup(repo, nil, 60)
		ctx := context.Background()
		if _, err := l.ByDistrict(ctx, "信義區"); err != nil {
			t.Fatal(err)
		}
		hit, err := l.ByDistrict(ctx, "信義區")
		if err != nil {
			t.Fatal(err)
		}
		hit[0].City = "被改寫"
		again, err := l.ByDistrict(ctx, "信義區")
		if err != nil {
			t.Fatal(err)
		}
		if again[0].City != "台北市" {
			t.Errorf("shared slice: got %q", again[0].City)
		}
		if repo.districtCalls != 1 {
			t.Errorf("repo calls = %d, want 1", repo.districtCalls)
		}
	})
}

// [F24-41] Redis 出錯時走本地快取.
func TestLookupLocalCacheWhenRedisErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("city+district hit and normalized key", func(t *testing.T) {
		repo := &countingRepo{cityRow: &weatherstore.WeatherForecast{City: "台北市", District: "信義區"}}
		l := NewLookup(repo, deadRedis(t), 60)
		for _, city := range []string{"臺北市", "臺北市", "台北市"} {
			f, err := l.ByCityAndDistrict(ctx, city, "信義區")
			if err != nil || f == nil || f.District != "信義區" {
				t.Fatalf("city %q: f=%v err=%v", city, f, err)
			}
		}
		if repo.cityCalls != 1 {
			t.Errorf("repo calls = %d, want 1", repo.cityCalls)
		}
	})

	t.Run("repo error not cached", func(t *testing.T) {
		repo := &countingRepo{err: errLookupRepoDown}
		l := NewLookup(repo, deadRedis(t), 60)
		for i := 0; i < 2; i++ {
			if _, err := l.ByCityAndDistrict(ctx, "台北市", "信義區"); !errors.Is(err, errLookupRepoDown) {
				t.Fatalf("call %d err = %v", i, err)
			}
		}
		if repo.cityCalls != 2 {
			t.Errorf("repo calls = %d, want 2", repo.cityCalls)
		}
	})

	t.Run("district cache separate from city cache", func(t *testing.T) {
		repo := &countingRepo{
			districtRows: districtRows(),
			cityRow:      &weatherstore.WeatherForecast{City: "台北市", District: "信義區"},
		}
		l := NewLookup(repo, deadRedis(t), 60)
		if _, err := l.ByDistrict(ctx, "信義區"); err != nil {
			t.Fatal(err)
		}
		if _, err := l.ByCityAndDistrict(ctx, "台北市", "信義區"); err != nil {
			t.Fatal(err)
		}
		if repo.cityCalls != 1 || repo.districtCalls != 1 {
			t.Errorf("calls district=%d city=%d, want 1/1", repo.districtCalls, repo.cityCalls)
		}
	})

	t.Run("district hit when redis errors", func(t *testing.T) {
		repo := &countingRepo{districtRows: districtRows()}
		l := NewLookup(repo, deadRedis(t), 60)
		for i := 0; i < 2; i++ {
			if _, err := l.ByDistrict(ctx, "信義區"); err != nil {
				t.Fatal(err)
			}
		}
		if repo.districtCalls != 1 {
			t.Errorf("repo calls = %d, want 1", repo.districtCalls)
		}
	})
}

// [F24-41] cacheTTLSeconds 為 0 時用預設 10 分鐘，不得 panic 也不得永不過期.
func TestLookupZeroTTLUsesDefault(t *testing.T) {
	tests := []struct {
		name    string
		seconds int
	}{
		{"zero", 0},
		{"negative", -5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &countingRepo{districtRows: districtRows()}
			l := NewLookup(repo, nil, tt.seconds)
			for i := 0; i < 2; i++ {
				if _, err := l.ByDistrict(context.Background(), "信義區"); err != nil {
					t.Fatal(err)
				}
			}
			if repo.districtCalls != 1 {
				t.Errorf("repo calls = %d, want 1", repo.districtCalls)
			}
		})
	}
	if defaultLookupTTL != 10*time.Minute {
		t.Errorf("defaultLookupTTL = %v, want 10m", defaultLookupTTL)
	}
}
