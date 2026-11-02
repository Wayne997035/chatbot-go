package integration

import (
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/recentmeal"
	"chatbot-go/internal/weather"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	weatherstore "chatbot-go/internal/storage/database/weather"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func setupTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start redis: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("redis host: %v", err)
	}
	port, err := c.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("redis port: %v", err)
	}
	client := redis.NewClient(&redis.Options{Addr: fmt.Sprintf("%s:%s", host, port.Port())})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestRedisHistoryKeepsRecentTurnsInOrder(t *testing.T) {
	client := setupTestRedis(t)
	ctx := context.Background()
	h := assistant.NewRedisHistory(client)

	for i := 1; i <= 4; i++ {
		h.Append(ctx, "U1", assistant.Turn{Question: fmt.Sprintf("q%d", i), Answer: fmt.Sprintf("a%d", i)})
	}
	turns := h.Recent(ctx, "U1")
	if len(turns) != 3 || turns[0].Question != "q2" || turns[2].Answer != "a4" {
		t.Fatalf("turns = %+v", turns)
	}
	if ttl := client.TTL(ctx, "qa:hist:U1").Val(); ttl <= 0 || ttl > 30*time.Minute {
		t.Errorf("ttl = %v", ttl)
	}
	if len(h.Recent(ctx, "U2")) != 0 {
		t.Error("histories must be per user")
	}
	if err := h.Clear(ctx, "U1"); err != nil {
		t.Fatal(err)
	}
	if len(h.Recent(ctx, "U1")) != 0 {
		t.Error("Clear must remove the history")
	}
}

type oneForecastRepo struct{}

func (oneForecastRepo) FindByDistrict(context.Context, string) ([]weatherstore.WeatherForecast, error) {
	return nil, nil
}

func (oneForecastRepo) FindByDistrictAndCity(_ context.Context, district, city string) (*weatherstore.WeatherForecast, error) {
	return &weatherstore.WeatherForecast{City: city, District: district, Elements: []weatherstore.ForecastElement{
		{ElementName: "Wx", Description: "天氣現象", Value: "多雲"},
		{ElementName: "PoP12h", Description: "12小時降雨機率", Value: "20"},
		{ElementName: "WeatherDescription", Description: "天氣預報綜合描述", Value: "很長的描述"},
	}}, nil
}

func (oneForecastRepo) FindAllDistricts(context.Context) ([]weatherstore.DistrictEntry, error) {
	return nil, nil
}

func (oneForecastRepo) Upsert(context.Context, *weatherstore.WeatherForecast) error { return nil }

func TestUserLocationsRememberAndSummarize(t *testing.T) {
	client := setupTestRedis(t)
	ctx := context.Background()
	locs := weather.NewUserLocations(client, weather.NewLookup(oneForecastRepo{}, nil, 60))

	if got := locs.ForUser(ctx, "U1"); got != "" {
		t.Fatalf("unknown user should have no weather, got %q", got)
	}
	locs.Remember(ctx, "U1", "台北市", "信義區")
	got := locs.ForUser(ctx, "U1")
	if !strings.Contains(got, "台北市信義區") || !strings.Contains(got, "天氣現象 多雲") || !strings.Contains(got, "降雨機率 20%") ||
		strings.Contains(got, "很長的描述") {
		t.Errorf("summary = %q", got)
	}
	if err := locs.Forget(ctx, "U1"); err != nil {
		t.Fatal(err)
	}
	if locs.ForUser(ctx, "U1") != "" {
		t.Error("Forget must remove the location")
	}
}

func TestRecentMealStoreOnRedis(t *testing.T) {
	client := setupTestRedis(t)
	ctx := context.Background()
	s := recentmeal.New(client)

	s.Remember(ctx, "U1", "m1", "text")
	got, ok := s.Get(ctx, "U1")
	if !ok || got.ID != "m1" || got.Source != "text" {
		t.Fatalf("get = %+v %v", got, ok)
	}
	if _, ok := s.Get(ctx, "U2"); ok {
		t.Error("keys must be per user")
	}
	if ttl := client.TTL(ctx, recentmeal.KeyPrefix+"U1").Val(); ttl <= 0 || ttl > recentmeal.TTL {
		t.Errorf("ttl = %v", ttl)
	}
	s.Remember(ctx, "U1", "m2", "photo")
	if got, _ := s.Get(ctx, "U1"); got.ID != "m2" {
		t.Errorf("the newest meal replaces the old one: %+v", got)
	}
	if err := s.Forget(ctx, "U1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(ctx, "U1"); ok || client.Exists(ctx, recentmeal.KeyPrefix+"U1").Val() != 0 {
		t.Error("forget removes the key")
	}
	if err := client.Set(ctx, recentmeal.KeyPrefix+"U3", "not json", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(ctx, "U3"); ok {
		t.Error("a corrupted value counts as no recent meal")
	}
}
