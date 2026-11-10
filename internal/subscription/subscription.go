// Package subscription 是通知訂閱的唯一讀寫入口：LIFF 與聊天端都經它，防寫回與地區正規化只寫一份.
//
// [F30-01] 本套件不得 import webhook（webhook 反過來用它）；刪除標記查詢由呼叫端以 ErasureChecker 注入.
package subscription

import (
	"chatbot-go/internal/region"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 錯誤分類；呼叫端一律用 errors.Is 判斷，不比對字串.
var (
	// ErrInvalidType 不是 alertsub.TypeEarthquakeTsunami / TypeWeatherWarning.
	ErrInvalidType = errors.New("invalid alert type")
	// ErrInvalidRegions 空、含不認得的地區、或超過上限.
	ErrInvalidRegions = errors.New("invalid regions")
	// ErrErased 刪除標記不早於 since：寫入前已被擋，或寫入後已補刪這一筆.
	ErrErased = errors.New("data erased")
	// ErrEraseIncomplete 補刪失敗，資料可能還在.
	ErrEraseIncomplete = errors.New("erasure undo failed")
	// ErrListUnsupported 底層 store 不支援讀取（型別斷言失敗）；不回空清單，避免把「讀不到」誤報成「沒開」.
	ErrListUnsupported = errors.New("subscription store cannot list")
)

// MaxRawRegions 原始 regions 陣列長度上限（去重前）. 去重後的合法最大值是 390（22 縣市 + 368 鄉鎮），400 不會擋掉合法輸入.
const MaxRawRegions = 400

// undoTimeout 補刪的期限；不跟著請求 ctx 走，請求結束或取消時補刪仍要做完.
const undoTimeout = 5 * time.Second

// Item 一項訂閱的現況.
type Item struct {
	AlertType string
	Enabled   bool
	// Regions 地震海嘯為空 slice（非 nil）；天氣特報為縣市或「縣市+鄉鎮」，一律用「台」字.
	Regions []string
}

// Service 通知訂閱的唯一入口.
type Service interface {
	// List 回該使用者每一筆已存在、且型別為已知兩種的訂閱（含 Enabled=false 的舊資料）；weather_warning 在前、earthquake_tsunami 在後.
	List(ctx context.Context, userID string) ([]Item, error)
	// EnableQuakeTsunami 開啟地震海嘯.
	EnableQuakeTsunami(ctx context.Context, userID string, since time.Time) error
	// SetWeatherWarningRegions 開啟天氣特報並整份取代地區；伺服器端驗證並正規化成 region.Counties 固定順序.
	SetWeatherWarningRegions(ctx context.Context, userID string, regions []string, since time.Time) error
	// Disable 刪除該筆訂閱文件；不存在也回 nil（冪等）.
	Disable(ctx context.Context, userID, alertType string) error
}

// ErasureChecker 刪除時間不早於 since 嗎（刪除標記由 webhook 擁有，這裡借用以避免建構循環）.
type ErasureChecker interface {
	ErasedSince(ctx context.Context, userID string, since time.Time) bool
}

// UndoContexter 選配：ErasureChecker 若也實作它，補刪改用它給的 ctx（例如聊天端整批事件共用的關機預算）.
type UndoContexter interface {
	UndoContext(ctx context.Context) (context.Context, context.CancelFunc)
}

// Store 窄介面：webhook 現有的 SubscriptionStore 與其測試假物件不用改就能傳進來.
type Store interface {
	Upsert(ctx context.Context, sub *alertsub.AlertSubscription) error
	Delete(ctx context.Context, id string) error
}

// Lister 選配：*alertsub.AlertSubRepo 有.
type Lister interface {
	FindByUserID(ctx context.Context, userID string) ([]alertsub.AlertSubscription, error)
}

// DefaultService 以 Store 實作 Service.
type DefaultService struct {
	repo   Store
	erased ErasureChecker
	now    func() time.Time
}

// New 建構；List 需要的讀取以型別斷言取得（repo 不是 Lister 時 List 回 ErrListUnsupported）.
func New(repo Store) *DefaultService {
	return &DefaultService{repo: repo, now: time.Now}
}

// WithErasureCheck 注入刪除標記查詢；nil 表示寫入前後都不查.
func (s *DefaultService) WithErasureCheck(e ErasureChecker) *DefaultService {
	s.erased = e
	return s
}

func subID(userID, alertType string) string { return userID + "_" + alertType }

// List 見 Service.
func (s *DefaultService) List(ctx context.Context, userID string) ([]Item, error) {
	l, ok := s.repo.(Lister)
	if !ok {
		return nil, ErrListUnsupported
	}
	subs, err := l.FindByUserID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	byType := map[string]Item{}
	for i := range subs {
		switch subs[i].AlertType {
		case alertsub.TypeWeatherWarning, alertsub.TypeEarthquakeTsunami:
			regions := append([]string{}, subs[i].Regions...)
			byType[subs[i].AlertType] = Item{AlertType: subs[i].AlertType, Enabled: subs[i].Enabled, Regions: regions}
		}
	}
	items := make([]Item, 0, len(byType))
	for _, t := range []string{alertsub.TypeWeatherWarning, alertsub.TypeEarthquakeTsunami} {
		if it, ok := byType[t]; ok {
			items = append(items, it)
		}
	}
	return items, nil
}

// EnableQuakeTsunami 見 Service.
func (s *DefaultService) EnableQuakeTsunami(ctx context.Context, userID string, since time.Time) error {
	now := s.now()
	return s.write(ctx, userID, since, &alertsub.AlertSubscription{
		ID: subID(userID, alertsub.TypeEarthquakeTsunami), UserID: userID, AlertType: alertsub.TypeEarthquakeTsunami,
		Regions: []string{}, Enabled: true, CreateTime: now, UpdateTime: now,
	})
}

// SetWeatherWarningRegions 見 Service.
func (s *DefaultService) SetWeatherWarningRegions(ctx context.Context, userID string, regions []string, since time.Time) error {
	norm, err := NormalizeRegions(regions)
	if err != nil {
		return err
	}
	now := s.now()
	return s.write(ctx, userID, since, &alertsub.AlertSubscription{
		ID: subID(userID, alertsub.TypeWeatherWarning), UserID: userID, AlertType: alertsub.TypeWeatherWarning,
		Regions: norm, Enabled: true, CreateTime: now, UpdateTime: now,
	})
}

// Disable 見 Service.
func (s *DefaultService) Disable(ctx context.Context, userID, alertType string) error {
	if alertType != alertsub.TypeWeatherWarning && alertType != alertsub.TypeEarthquakeTsunami {
		return ErrInvalidType
	}
	if err := s.repo.Delete(ctx, subID(userID, alertType)); err != nil {
		return fmt.Errorf("disable subscription: %w", err)
	}
	return nil
}

// write 寫入落地前後都守刪除標記：寫前命中不寫；寫後（成功或回錯都）命中就補刪剛寫的這一筆.
// 錯誤文字不含 userID 與地區字串.
func (s *DefaultService) write(ctx context.Context, userID string, since time.Time, sub *alertsub.AlertSubscription) error {
	if s.erased != nil && s.erased.ErasedSince(ctx, userID, since) {
		return ErrErased
	}
	upsertErr := s.repo.Upsert(ctx, sub)
	if s.erased != nil && s.erased.ErasedSince(ctx, userID, since) {
		uctx, cancel := s.undoContext(ctx)
		defer cancel()
		if err := s.repo.Delete(uctx, sub.ID); err != nil {
			// 保留底層錯誤給呼叫端 log（呼叫端必須經 redact）；errors.Is 仍認得 ErrEraseIncomplete.
			return fmt.Errorf("%w: %w", ErrEraseIncomplete, err)
		}
		return ErrErased
	}
	if upsertErr != nil {
		return fmt.Errorf("save subscription: %w", upsertErr)
	}
	return nil
}

func (s *DefaultService) undoContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if u, ok := s.erased.(UndoContexter); ok {
		return u.UndoContext(ctx)
	}
	return context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
}

// NormalizeRegions 驗證並正規化天氣特報地區：
// ① 原始長度 > MaxRawRegions → ErrInvalidRegions；② 臺→台，每項須是縣市或「縣市+鄉鎮」；③ 去重，整縣吃掉底下鄉鎮；
// ④ 空 → ErrInvalidRegions；⑤ 輸出依 region.Counties 順序、同縣市整縣獨佔否則依 region.Townships 順序.
func NormalizeRegions(raw []string) ([]string, error) {
	if len(raw) > MaxRawRegions {
		return nil, ErrInvalidRegions
	}
	whole := map[string]bool{}
	areas := map[string]bool{}
	for _, r := range raw {
		r = strings.ReplaceAll(r, "臺", "台")
		if region.IsCounty(r) {
			whole[r] = true
			continue
		}
		if _, _, ok := region.SplitArea(r); !ok {
			return nil, ErrInvalidRegions
		}
		areas[r] = true
	}
	out := []string{}
	for _, c := range region.Counties {
		if whole[c] {
			out = append(out, c)
			continue
		}
		for _, t := range region.Townships(c) {
			if areas[c+t] {
				out = append(out, c+t)
			}
		}
	}
	if len(out) == 0 {
		return nil, ErrInvalidRegions
	}
	return out, nil
}
