package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// StateType 對話狀態類型.
type StateType string

const (
	// TypeWeatherRegionSelect 天氣特報地區選擇狀態.
	TypeWeatherRegionSelect StateType = "weather_region_select"

	// TypeLocationClarify 地點澄清狀態（多候選地點時使用）.
	TypeLocationClarify StateType = "location_clarify"

	stateTTL  = 5 * time.Minute
	keyPrefix = "conv:"
)

// CandidateLocation 候選地點.
type CandidateLocation struct {
	City        string `json:"city"`
	District    string `json:"district"`
	DisplayName string `json:"displayName"`
	Source      string `json:"source"` // "fuzzy" | "geocode"
}

// State 對話狀態.
type State struct {
	Type       StateType           `json:"type"`
	Regions    []string            `json:"regions"`
	Candidates []CandidateLocation `json:"candidates,omitempty"`
}

// Manager 對話狀態管理器.
type Manager struct {
	redisClient *redis.Client
	ttl         time.Duration
}

// NewManager 建立對話狀態管理器.
func NewManager(redisClient *redis.Client) *Manager {
	return &Manager{
		redisClient: redisClient,
		ttl:         stateTTL,
	}
}

// Get 取得使用者對話狀態；找不到或 Redis 不可用時回傳 nil, nil.
func (m *Manager) Get(ctx context.Context, userID string) (*State, error) {
	if m.redisClient == nil {
		return nil, nil
	}

	key := keyPrefix + userID
	val, err := m.redisClient.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, fmt.Errorf("get conversation state: %w", err)
	}

	var state State
	if err := json.Unmarshal([]byte(val), &state); err != nil {
		return nil, fmt.Errorf("decode conversation state: %w", err)
	}
	return &state, nil
}

// Set 儲存使用者對話狀態.
func (m *Manager) Set(ctx context.Context, userID string, state *State) error {
	if m.redisClient == nil {
		return nil
	}

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode conversation state: %w", err)
	}

	key := keyPrefix + userID
	if err := m.redisClient.Set(ctx, key, data, m.ttl).Err(); err != nil {
		return fmt.Errorf("set conversation state: %w", err)
	}
	return nil
}

// Delete 刪除使用者對話狀態.
func (m *Manager) Delete(ctx context.Context, userID string) error {
	if m.redisClient == nil {
		return nil
	}

	key := keyPrefix + userID
	if err := m.redisClient.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("delete conversation state: %w", err)
	}
	return nil
}
