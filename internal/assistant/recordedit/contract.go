// Package recordedit 聊天修改紀錄的純邏輯：修改計畫的解析、驗證、套用與待確認提議的儲存介面（無 I/O）.
package recordedit

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 修改操作（封閉列舉；AI 輸出不在這份清單內一律 Invalid）.
const (
	OpNone            = "none"
	OpSetTotalKcal    = "set_total_kcal"
	OpSetItemKcal     = "set_item_kcal"
	OpSetItemByWeight = "set_item_by_weight"
	OpScale           = "scale"
	OpAddItem         = "add_item"
	OpRemoveItem      = "remove_item"
	OpRenameItem      = "rename_item"
	OpDeleteMeal      = "delete_meal"
	OpSetWorkout      = "set_workout"
	OpDeleteWorkout   = "delete_workout"
	// [F34-10] 改餐別（整筆）與把品項移到別的餐別 / 拆成多餐.
	OpSetMealType = "set_meal_type"
	OpMoveItems   = "move_items"
)

// RecordKind 被修改的紀錄種類.
type RecordKind string

// 紀錄種類.
const (
	KindMeal    RecordKind = "meal"
	KindWorkout RecordKind = "workout"
)

// OutcomeKind Planner 的結果類別；零值是 OutcomeNone（沒有要改，交給原路由）.
type OutcomeKind int

// Planner 結果類別.
const (
	OutcomeNone OutcomeKind = iota
	OutcomeProposal
	OutcomeClarify
	OutcomeInvalid
)

// 提議的存活時間與 nonce 長度.
const (
	PendingTTL = 5 * time.Minute
	NonceLen   = 22 // 16 bytes 的 base64url（無 padding）
	// MaxBackdateDays DoneAt 最多往前改幾天（含今天共這麼多天）；與 LIFF 統計頁可查範圍 liff.maxDays 同值.
	MaxBackdateDays = 90
)

var (
	// ErrNotFound 沒有待確認的提議、已過期、已被取走，或 nonce 不符（呼叫端一律回「已過期或已處理過」）.
	ErrNotFound = errors.New("recordedit: pending edit not found")
	// ErrInvalidPlan 計畫不合法（未知 op、缺欄位、型別錯）.
	ErrInvalidPlan = errors.New("recordedit: invalid plan")
	// ErrOutOfRange 數值超出範圍.
	ErrOutOfRange = errors.New("recordedit: value out of range")
)

// NewItem add_item 的新品項（由 planner 同一次呼叫提供，程式驗範圍）.
type NewItem struct {
	Name     string  `json:"name"`
	Portion  string  `json:"portion"`
	Calories float64 `json:"calories"`
	ProteinG float64 `json:"protein_g"`
	CarbsG   float64 `json:"carbs_g"`
	FatG     float64 `json:"fat_g"`
}

// Plan AI 唯一的輸出；欄位語意與範圍見規格 §4、§5.
type Plan struct {
	Op            string   `json:"op"`
	Target        string   `json:"target"` // 編號表代號（M3、W2），不是 DB ID
	Item          int      `json:"item"`   // 1 起算
	Kcal          float64  `json:"kcal"`
	Grams         float64  `json:"grams"`
	KcalPer100G   float64  `json:"kcal_per_100g"`
	Factor        float64  `json:"factor"`
	NewItem       *NewItem `json:"new_item,omitempty"`
	Name          string   `json:"name"`
	Portion       string   `json:"portion"`
	DurationMin   float64  `json:"duration_min"`              // 0 = 不改
	DistanceKM    float64  `json:"distance_km"`               // 0 = 不改
	DoneDayOffset *int     `json:"done_day_offset,omitempty"` // 0 今天、-1 昨天…；nil = 不改日期
	DoneTime      string   `json:"done_time"`                 // "HH:MM"（台北時間）；空 = 不改時刻
	Candidates    []string `json:"candidates,omitempty"`
	Basis         string   `json:"basis"`
	// [F34-10] set_meal_type 的目的餐別、move_items 的分組（target 永遠是來源那一筆，併入哪一筆由程式決定）.
	MealType string `json:"meal_type"`
	Moves    []Move `json:"moves,omitempty"`
}

// Move [F34-10] move_items 的一組：來源品項序號（1 起算）要去哪個餐別.
type Move struct {
	Items    []int  `json:"items"`
	MealType string `json:"meal_type"`
}

// ParsePlan 取第一個完整的 JSON 物件並驗證：op 在列舉內、必要欄位齊全、型別正確；op=none 回 (plan, nil).
// 實作在 plan.go.

// WorkoutResult 運動提議的最終值（提議當下由程式算好；確認時照這個寫，不重算）.
type WorkoutResult struct {
	DurationMin float64   `json:"durationMin"`
	DistanceKM  float64   `json:"distanceKm"`
	Calories    float64   `json:"calories"`
	DoneAt      time.Time `json:"doneAt"`
}

// Proposal 一筆已驗證、可請客人確認的修改；Summary 是程式樣板組出的確認文字，不含 AI 自由句.
type Proposal struct {
	Kind        RecordKind     `json:"kind"`
	Op          string         `json:"op"`
	RecordID    string         `json:"recordId"`    // 來自編號表反查或候選，從不來自 AI 或 postback
	BaseVersion int64          `json:"baseVersion"` // 提議當下紀錄的 Version；確認時不符就拒絕
	Plan        Plan           `json:"plan"`
	Workout     *WorkoutResult `json:"workout,omitempty"`
	Summary     string         `json:"summary"`
	Before      float64        `json:"before"` // 熱量（刪除時為被刪紀錄的熱量）
	After       float64        `json:"after"`
	// Regroup [F34-12] 改餐別 / 移品項 / 拆多餐的寫入明細（提議當下固定，確認時不重算 EatenAt 與併入目標）；舊暫存沒有這個欄位.
	Regroup *RegroupSpec `json:"regroup,omitempty"`
}

// RegroupSpec [F34-12] 一次改餐別 / 移品項 / 拆多餐的寫入明細.
type RegroupSpec struct {
	SourceID   string
	SourceBase int64
	InPlace    bool // 原地改（只改來源的 MealType 與 EatenAt，Items/Total/Scale/Base 位元不變）
	Groups     []RegroupGroup
	Emptied    bool // 來源 remaining 為空且非原地 → 刪來源
}

// RegroupGroup [F34-12] 一組：來源品項序號（1 起算）去哪個餐別、併入哪一筆（空 = 新開）.
type RegroupGroup struct {
	MealType  string
	Items     []int
	Stay      bool // 目的餐別等於來源餐別：留在原處
	MergeID   string
	MergeBase int64
	EatenAt   time.Time // 新開 / 原地改用
}

// BuildMealProposal 在餐點副本上試算 plan 並產生提議；入參 m 不被修改；超出範圍回包裝 ErrOutOfRange 的錯誤.
// 實作在 meal_build.go.

// BuildWorkoutProposal 同上；weightKG 供重算熱量，now 供 DoneAt 驗證（不得晚於 now、不得早於 MinDoneAt(now)）.
// 實作在 workout_build.go.

// ApplyMeal 把提議套到（最新讀出的）餐點；失敗時 m 不被改動.
// 實作在 meal_build.go.

// ApplyWorkout 把提議的最終值寫進運動；失敗時 w 不被改動.
// 實作在 workout_build.go.

// MinDoneAt DoneAt 的下限：台北時間今天 00:00 往前 MaxBackdateDays-1 天.
// 實作在 workout_build.go.

// Candidate 多筆符合時的候選.
type Candidate struct {
	RecordID string `json:"recordId"`
	Label    string `json:"label"` // 已清理的顯示文字（日期餐別＋熱量），≤20 字
}

// Outcome Planner 的結果；Clarify 時 Plan 與 RecordKind 保留，客人點選候選後用 Build*Proposal 產生提議.
type Outcome struct {
	Kind       OutcomeKind
	Proposal   *Proposal   // OutcomeProposal
	Plan       *Plan       // OutcomeClarify
	RecordKind RecordKind  // OutcomeClarify
	Candidates []Candidate // OutcomeClarify，2–5 筆
	Reason     string      // OutcomeNone / OutcomeInvalid，只給 log（不含使用者文字）
}

// Planner 由 *assistant.Assistant 實作；webhook 只依賴這個介面.
type Planner interface {
	PlanEdit(ctx context.Context, userID, text string) (*Outcome, error)
}

// Pending 一個使用者目前唯一的待確認項目（提議或候選）.
type Pending struct {
	Nonce      string      `json:"nonce"`
	Kind       OutcomeKind `json:"kind"` // OutcomeProposal 或 OutcomeClarify
	Proposal   *Proposal   `json:"proposal,omitempty"`
	Plan       *Plan       `json:"plan,omitempty"`
	RecordKind RecordKind  `json:"recordKind,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`
	ExpiresAt  time.Time   `json:"expiresAt"`
}

// Store 待確認項目的暫存；每個 userID 最多一筆，所有方法只認傳入的 userID.
// 實作：Redis（有設定時）、MongoDB（沒有 Redis 時）；兩者由同一組契約測試規範.
type Store interface {
	// Put 存入並覆蓋該使用者既有的待確認項目；ExpiresAt 由呼叫端設為 now+PendingTTL.
	Put(ctx context.Context, userID string, p *Pending) error
	// Take 原子地「比對 nonce、檢查未過期、取出並刪除」；nonce 不符、過期、不存在一律 ErrNotFound，
	// 且 nonce 不符時不得刪掉原本有效的項目（亂送 nonce 不能毀掉客人的提議）.
	Take(ctx context.Context, userID, nonce string, now time.Time) (*Pending, error)
	// Delete 刪除該使用者的待確認項目（刪除資料用）；不存在不算錯.
	Delete(ctx context.Context, userID string) error
	// PeekPending [F34-16] 不消耗地讀出該使用者目前未過期的待確認項目（打字「確認」「取消」用，先看有沒有再 Take）；
	// 沒有、過期、他人的一律 ErrNotFound. 與 Take 同一個過期判斷，讀出後 Take 仍可成功.
	PeekPending(ctx context.Context, userID string, now time.Time) (*Pending, error)
}

// NewNonce 16 bytes crypto/rand → base64url（無 padding，22 字元）.
func NewNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("new nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ValidNonce 長度與字元集檢查（只是格式；是否存在由 Store.Take 決定）.
func ValidNonce(s string) bool {
	if len(s) != NonceLen {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// Postback 動作.
const (
	ActionOK     = "ok"
	ActionCancel = "no"
	ActionPick   = "pick"
)

const postbackPrefix = "ce1"

// Postback 解析後的按鈕資料（不含任何紀錄 ID 或數值）.
type Postback struct {
	Action string
	Nonce  string
	Index  int // 只有 ActionPick 有意義，0–4
}

// PostbackData 組按鈕資料：ce1:ok:<nonce>、ce1:no:<nonce>、ce1:pick:<nonce>:<i>.
func PostbackData(action, nonce string, index int) string {
	d := postbackPrefix + ":" + action + ":" + nonce
	if action == ActionPick {
		d += fmt.Sprintf(":%d", index)
	}
	return d
}

// ParsePostback 嚴格解析；格式錯、nonce 格式錯、index 不是 0–4、長度超過 64 一律 ok=false.
func ParsePostback(data string) (Postback, bool) {
	if len(data) > 64 {
		return Postback{}, false
	}
	parts := strings.Split(data, ":")
	if len(parts) < 3 || parts[0] != postbackPrefix || !ValidNonce(parts[2]) {
		return Postback{}, false
	}
	switch parts[1] {
	case ActionOK, ActionCancel:
		if len(parts) != 3 {
			return Postback{}, false
		}
		return Postback{Action: parts[1], Nonce: parts[2]}, true
	case ActionPick:
		if len(parts) != 4 || len(parts[3]) != 1 || parts[3][0] < '0' || parts[3][0] > '4' {
			return Postback{}, false
		}
		return Postback{Action: ActionPick, Nonce: parts[2], Index: int(parts[3][0] - '0')}, true
	}
	return Postback{}, false
}
