package conversation

import (
	"chatbot-go/internal/platform/localcache"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

	// maxLocalStates 本地備援最多保留的狀態數（localcache 容量）.
	maxLocalStates = 1000

	// memoryBackendMax NewMemoryManager 假 Redis 的容量，避免測試中滿載拒收.
	memoryBackendMax = 10000
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
	backend store                         // Redis；未設定時為 nil
	local   *localcache.Cache[localEntry] // Redis 未設定或出錯時使用
	ttl     time.Duration
}

// localEntry 本地備援中的一筆狀態.
type localEntry struct {
	value   string
	deleted bool // [F24-04] 刪除標記：遮蔽 Redis 裡尚未刪掉的舊值，直到過期
}

// newManager 建立管理器；now 供測試注入時鐘.
// [F24-31] 本地備援用 localcache（純快取、有容量）取代手寫 map.
func newManager(backend store, now func() time.Time) *Manager {
	return &Manager{
		backend: backend,
		local:   localcache.NewWithClock[localEntry]("conversation", maxLocalStates, stateTTL, now),
		ttl:     stateTTL,
	}
}

// NewManager 建立對話狀態管理器；redisClient 為 nil 時狀態存在程式內記憶體（重啟或休眠後遺失，只適用單一執行個體）.
func NewManager(redisClient *redis.Client) *Manager {
	// [F24-01] 一律備好記憶體後端，Redis 未設定或出錯時使用
	if redisClient != nil {
		return newManager(redisStore{client: redisClient}, time.Now)
	}
	slog.Warn("conversation state: Redis not configured, keeping state in memory")
	return newManager(nil, time.Now)
}

// NewMemoryManager 建立只存在記憶體的對話狀態管理器（測試用）；另掛一份記憶體當假 Redis，讓需要共享儲存的狀態也能存取.
func NewMemoryManager() *Manager {
	fake := cacheStore{c: localcache.NewWithClock[string]("conversation_shared_fake", memoryBackendMax, stateTTL, time.Now)}
	return newManager(fake, time.Now)
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
	// [F24-04] 本地有值就是最後一次寫入（Redis 出錯期間存的），優先於 Redis；刪除標記回 nil
	entry, found := m.local.Get(key)
	if found && entry.deleted {
		return nil, nil
	}
	val := entry.value
	if !found && m.backend != nil {
		v, ok, err := m.backend.get(ctx, key)
		if err != nil {
			// [F24-02] Redis 讀取出錯，視為沒有
			slog.Warn("conversation state: Redis get failed", "error", err)
			return nil, nil
		}
		if !ok {
			return nil, nil
		}
		val, found = v, true
	}
	if !found {
		return nil, nil
	}
	var state State
	if err := json.Unmarshal([]byte(val), &state); err != nil {
		return nil, fmt.Errorf("decode conversation state: %w", err)
	}
	return &state, nil
}

// Set 儲存使用者對話狀態；刪除確認只存共享的 Redis，沒有 Redis 或寫入失敗時回 error.
func (m *Manager) Set(ctx context.Context, userID string, state *State) error {
	if !m.Available() {
		return nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode conversation state: %w", err)
	}
	key := keyPrefix + userID
	// [F24-05] 不可逆操作的確認狀態不能只存在單台記憶體
	needShared := state.Type == TypeDeleteDataConfirm
	if m.backend == nil {
		if needShared {
			return fmt.Errorf("set conversation state: %w", errSharedStoreRequired)
		}
		m.local.SetTTL(key, localEntry{value: string(data)}, m.ttl)
		return nil
	}
	// [F24-02] Redis 出錯時退回記憶體；Redis 寫入成功時清掉記憶體舊狀態與刪除標記
	if err := m.backend.set(ctx, key, string(data), m.ttl); err != nil {
		if needShared {
			return fmt.Errorf("set conversation state: %w", err)
		}
		slog.Warn("conversation state: Redis set failed, using memory", "error", err)
		m.local.SetTTL(key, localEntry{value: string(data)}, m.ttl)
		return nil
	}
	m.local.Delete(key)
	return nil
}

// Delete 刪除使用者對話狀態.
func (m *Manager) Delete(ctx context.Context, userID string) error {
	if !m.Available() {
		return nil
	}
	key := keyPrefix + userID
	// [F24-02] 記憶體那份一定先刪
	m.local.Delete(key)
	if m.backend != nil {
		if err := m.backend.del(ctx, key); err != nil {
			// [F24-04] Redis 舊值還在；留刪除標記，Get 之後不會讀回它
			m.local.SetTTL(key, localEntry{deleted: true}, m.ttl)
			return fmt.Errorf("delete conversation state: %w", err)
		}
	}
	return nil
}

// errSharedStoreRequired 該狀態必須存在共享儲存（Redis），但沒有設定.
var errSharedStoreRequired = errors.New("shared store (Redis) required")

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

// cacheStore 以 localcache 實作的 store（NewMemoryManager 的假 Redis）.
type cacheStore struct {
	c *localcache.Cache[string]
}

func (s cacheStore) get(_ context.Context, key string) (value string, found bool, err error) {
	value, found = s.c.Get(key)
	return value, found, nil
}

func (s cacheStore) set(_ context.Context, key, value string, ttl time.Duration) error {
	s.c.SetTTL(key, value, ttl)
	return nil
}

func (s cacheStore) del(_ context.Context, key string) error {
	s.c.Delete(key)
	return nil
}
