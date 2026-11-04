package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

	// TypeMealClarify 打字記餐太籠統，等使用者補充（或補照片）.
	TypeMealClarify StateType = "meal_clarify"

	// TypeMealPlace 等使用者說這餐在哪裡吃（輸入店名或傳位置）.
	TypeMealPlace StateType = "meal_place"

	stateTTL  = 5 * time.Minute
	keyPrefix = "conv:"

	// maxLocalStates 記憶體版超過這個數量才清理過期狀態.
	maxLocalStates = 1000
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

// PendingMeal 打字記餐的進行中資料.
type PendingMeal struct {
	Text      string `json:"text,omitempty"`     // 使用者原本的描述（追問時）
	MealType  string `json:"mealType,omitempty"` // 第一次估算時判斷的餐別
	DayOffset int    `json:"dayOffset,omitempty"`
	MealID    string `json:"mealId,omitempty"` // 要補店家的那一餐
}

// State 對話狀態.
type State struct {
	Type       StateType           `json:"type"`
	Regions    []string            `json:"regions"`
	Candidates []CandidateLocation `json:"candidates,omitempty"`
	Workout    *PendingWorkout     `json:"workout,omitempty"`
	Meal       *PendingMeal        `json:"meal,omitempty"`
}

// store 狀態儲存後端（Redis）.
type store interface {
	get(ctx context.Context, key string) (string, bool, error)
	set(ctx context.Context, key, value string, ttl time.Duration) error
	del(ctx context.Context, key string) error
}

// Manager 對話狀態管理器.
type Manager struct {
	backend store        // Redis；未設定時為 nil
	local   *memoryStore // Redis 未設定或出錯時使用
	ttl     time.Duration
}

// NewManager 建立對話狀態管理器；redisClient 為 nil 時狀態存在程式內記憶體（重啟或休眠後遺失，只適用單一執行個體）.
func NewManager(redisClient *redis.Client) *Manager {
	// [F24-01] 一律備好記憶體後端，Redis 未設定或出錯時使用
	m := &Manager{local: newMemoryStore(), ttl: stateTTL}
	if redisClient != nil {
		m.backend = redisStore{client: redisClient}
	} else {
		slog.Warn("conversation state: Redis not configured, keeping state in memory")
	}
	return m
}

// NewMemoryManager 建立只存在記憶體的對話狀態管理器（測試用）.
func NewMemoryManager() *Manager {
	return &Manager{local: newMemoryStore(), ttl: stateTTL}
}

// Available 是否能保存多輪對話狀態.
func (m *Manager) Available() bool {
	return m != nil && m.local != nil
}

// Get 取得使用者對話狀態；找不到或無法保存狀態時回傳 nil, nil.
func (m *Manager) Get(ctx context.Context, userID string) (*State, error) {
	if !m.Available() {
		return nil, nil
	}
	key := keyPrefix + userID
	val, ok := "", false
	if m.backend != nil {
		v, found, err := m.backend.get(ctx, key)
		if err != nil {
			// [F24-02] Redis 讀取出錯，退回記憶體
			slog.Warn("conversation state: Redis get failed, using memory", "error", err)
		} else if found {
			val, ok = v, true
		}
	}
	if !ok {
		// [F24-01] 沒 Redis 或 Redis 沒有；Redis 出錯期間存的狀態在記憶體
		v, found, err := m.local.get(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("get conversation state: %w", err)
		}
		if !found {
			return nil, nil
		}
		val = v
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
	key := keyPrefix + userID
	// [F24-02] Redis 出錯時退回記憶體；Redis 寫入成功時清掉記憶體舊狀態
	if m.backend != nil {
		err := m.backend.set(ctx, key, string(data), m.ttl)
		if err == nil {
			_ = m.local.del(ctx, key) // Redis 寫入成功,清掉出錯期間留在記憶體的舊狀態
			return nil
		}
		slog.Warn("conversation state: Redis set failed, using memory", "error", err)
	}
	_ = m.local.set(ctx, key, string(data), m.ttl)
	return nil
}

// Delete 刪除使用者對話狀態.
func (m *Manager) Delete(ctx context.Context, userID string) error {
	if !m.Available() {
		return nil
	}
	key := keyPrefix + userID
	// [F24-02] 記憶體那份一定先刪
	_ = m.local.del(ctx, key)
	if m.backend != nil {
		if err := m.backend.del(ctx, key); err != nil {
			return fmt.Errorf("delete conversation state: %w", err)
		}
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

// memoryEntry 記憶體中的一筆狀態.
type memoryEntry struct {
	value   string
	expires time.Time
}

// memoryStore 程式內記憶體；Redis 未設定或出錯時使用，狀態有 TTL，超量時清掉過期的.
type memoryStore struct {
	mu   sync.Mutex
	now  func() time.Time
	data map[string]memoryEntry
}

func newMemoryStore() *memoryStore {
	return &memoryStore{now: time.Now, data: map[string]memoryEntry{}}
}

func (s *memoryStore) get(_ context.Context, key string) (value string, found bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[key]
	if !ok {
		return "", false, nil
	}
	if !s.now().Before(e.expires) {
		delete(s.data, key)
		return "", false, nil
	}
	return e.value, true, nil
}

func (s *memoryStore) set(_ context.Context, key, value string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data) >= maxLocalStates {
		s.pruneLocked()
	}
	s.data[key] = memoryEntry{value: value, expires: s.now().Add(ttl)}
	return nil
}

// pruneLocked 刪除所有已過期的項目；呼叫端需持有 mu.
func (s *memoryStore) pruneLocked() {
	now := s.now()
	for k, e := range s.data {
		if !now.Before(e.expires) {
			delete(s.data, k)
		}
	}
}

func (s *memoryStore) del(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}
