package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
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

	// TypeWorkoutDuration 已選運動種類，等待輸入時間或距離.
	TypeWorkoutDuration StateType = "workout_duration"

	// TypeWorkoutConfirm AI 解析出的運動，等待使用者確認.
	TypeWorkoutConfirm StateType = "workout_confirm"

	// TypeDeleteDataConfirm 等待確認刪除個人健康資料.
	TypeDeleteDataConfirm StateType = "delete_data_confirm"

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

// PendingWorkout 尚未存檔的運動紀錄.
type PendingWorkout struct {
	Activity    string  `json:"activity"`
	DurationMin float64 `json:"durationMin,omitempty"`
	DistanceKM  float64 `json:"distanceKm,omitempty"`
	Calories    float64 `json:"calories,omitempty"`
}

// State 對話狀態.
type State struct {
	Type       StateType           `json:"type"`
	Regions    []string            `json:"regions"`
	Candidates []CandidateLocation `json:"candidates,omitempty"`
	Workout    *PendingWorkout     `json:"workout,omitempty"`
}

// store 狀態儲存後端（Redis；測試用記憶體）.
type store interface {
	get(ctx context.Context, key string) (string, bool, error)
	set(ctx context.Context, key, value string, ttl time.Duration) error
	del(ctx context.Context, key string) error
}

// Manager 對話狀態管理器.
type Manager struct {
	backend store
	ttl     time.Duration
}

// NewManager 建立對話狀態管理器；redisClient 為 nil 時不保存狀態（多輪對話停用）.
func NewManager(redisClient *redis.Client) *Manager {
	m := &Manager{ttl: stateTTL}
	if redisClient != nil {
		m.backend = redisStore{client: redisClient}
	}
	return m
}

// NewMemoryManager 建立存在記憶體的對話狀態管理器（測試用）.
func NewMemoryManager() *Manager {
	return &Manager{backend: &memoryStore{data: map[string]string{}}, ttl: stateTTL}
}

// Available 是否能保存多輪對話狀態.
func (m *Manager) Available() bool {
	return m != nil && m.backend != nil
}

// Get 取得使用者對話狀態；找不到或無法保存狀態時回傳 nil, nil.
func (m *Manager) Get(ctx context.Context, userID string) (*State, error) {
	if !m.Available() {
		return nil, nil
	}
	val, ok, err := m.backend.get(ctx, keyPrefix+userID)
	if err != nil {
		return nil, fmt.Errorf("get conversation state: %w", err)
	}
	if !ok {
		return nil, nil
	}
	var state State
	if err := json.Unmarshal([]byte(val), &state); err != nil {
		return nil, fmt.Errorf("decode conversation state: %w", err)
	}
	return &state, nil
}

// Set 儲存使用者對話狀態.
func (m *Manager) Set(ctx context.Context, userID string, state *State) error {
	if !m.Available() {
		return nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode conversation state: %w", err)
	}
	if err := m.backend.set(ctx, keyPrefix+userID, string(data), m.ttl); err != nil {
		return fmt.Errorf("set conversation state: %w", err)
	}
	return nil
}

// Delete 刪除使用者對話狀態.
func (m *Manager) Delete(ctx context.Context, userID string) error {
	if !m.Available() {
		return nil
	}
	if err := m.backend.del(ctx, keyPrefix+userID); err != nil {
		return fmt.Errorf("delete conversation state: %w", err)
	}
	return nil
}

type redisStore struct {
	client *redis.Client
}

func (r redisStore) get(ctx context.Context, key string) (value string, found bool, err error) {
	value, err = r.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (r redisStore) set(ctx context.Context, key, value string, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

func (r redisStore) del(ctx context.Context, key string) error {
	return r.client.Del(ctx, key).Err()
}

// memoryStore 測試用；不處理 TTL.
type memoryStore struct {
	mu   sync.Mutex
	data map[string]string
}

func (m *memoryStore) get(_ context.Context, key string) (value string, found bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, found = m.data[key]
	return value, found, nil
}

func (m *memoryStore) set(_ context.Context, key, value string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

func (m *memoryStore) del(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}
