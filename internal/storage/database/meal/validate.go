package meal

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// [F34-54] 手動修改餐點時的範圍. 取「本樹既有寫入路徑能存進去的聯集」，否則使用者打開自己的舊紀錄會存不回去.
// 單品熱量上限不在這裡另立：唯一定義是 MaxItemCalories（edit.go）.
const (
	MaxItems        = 20  // 每餐最多品項（舊資料品項數已超過時，以既有數量為上限，見 EditContext）
	MaxNameRunes    = 30  // 品名最多字數
	MaxPortionRunes = 30  // 份量最多字數（聊天可存 30，分析端 20，取大者）
	MaxMacroGrams   = 500 // 單項蛋白質 / 碳水 / 脂肪上限（克）
)

// 錯誤訊息裡的欄位名稱；測試用同一組常數.
const (
	FieldItems    = "items"
	FieldName     = "name"
	FieldPortion  = "portion"
	FieldCalories = "calories"
	FieldProtein  = "protein"
	FieldCarbs    = "carbs"
	FieldFat      = "fat"
)

// EditContext [F34-54] ValidateEdit 用：編輯既有餐點時，改前的狀態：沒改的品項免檢查，品項數與整餐熱量不得比改前更糟.
type EditContext struct {
	// Unchanged 與 items 等長；true = 這個品項與改前完全相同，免範圍檢查（舊資料可能有 6000 大卡的單品或 25 字份量）.
	Unchanged []bool
	// PriorCount 改前的品項數.
	PriorCount int
	// PriorCalories 改前的整餐熱量（品項加總）.
	PriorCalories float64
}

// ValidateItems 檢查一餐新品項是否在允許範圍內（所有品項都檢查），錯誤訊息指出是哪個欄位.
// containsURL 由呼叫端傳入網址判斷（meal 不能反向 import nutrition）；為 nil 視為程式錯誤，不會略過網址檢查.
func ValidateItems(items []FoodItem, containsURL func(string) bool) error {
	return validate(items, containsURL, nil)
}

// ValidateEdit [F34-54] 編輯既有餐點：同 ValidateItems，但 edit.Unchanged 的品項免檢查，品項數上限取 max(MaxItems, 改前品項數)，
// 整餐熱量超過 MaxTotalCalories 時只要不高於改前就放行.
func ValidateEdit(items []FoodItem, containsURL func(string) bool, edit EditContext) error {
	if len(edit.Unchanged) != len(items) {
		return errors.New("EditContext.Unchanged must be as long as items")
	}
	return validate(items, containsURL, &edit)
}

func validate(items []FoodItem, containsURL func(string) bool, edit *EditContext) error {
	if containsURL == nil {
		return errors.New("containsURL must not be nil")
	}
	maxItems := MaxItems
	if edit != nil {
		maxItems = max(MaxItems, edit.PriorCount)
	}
	if len(items) < 1 || len(items) > maxItems {
		return fmt.Errorf("%s: need 1 to %d, got %d", FieldItems, maxItems, len(items))
	}
	var total float64
	for i := range items {
		total += items[i].Calories
		if edit != nil && edit.Unchanged[i] {
			continue
		}
		if err := validateItem(&items[i], containsURL); err != nil {
			return fmt.Errorf("item %d: %w", i+1, err)
		}
	}
	// 整餐上限：不超過 MaxTotalCalories，或不高於改前（SetTotalCalories 可把單品推到 6000，舊資料合計可能已超過）.
	if total > MaxTotalCalories && (edit == nil || total > edit.PriorCalories) {
		return fmt.Errorf("%s: meal total %v out of range 0 to %d", FieldCalories, total, MaxTotalCalories)
	}
	return nil
}

func validateItem(it *FoodItem, containsURL func(string) bool) error {
	if strings.TrimSpace(it.Name) == "" {
		return fmt.Errorf("%s: must not be empty", FieldName)
	}
	if n := utf8.RuneCountInString(it.Name); n > MaxNameRunes {
		return fmt.Errorf("%s: %d characters, max %d", FieldName, n, MaxNameRunes)
	}
	if containsURL(it.Name) {
		return fmt.Errorf("%s: must not contain a URL", FieldName)
	}
	if n := utf8.RuneCountInString(it.Portion); n > MaxPortionRunes {
		return fmt.Errorf("%s: %d characters, max %d", FieldPortion, n, MaxPortionRunes)
	}
	if containsURL(it.Portion) {
		return fmt.Errorf("%s: must not contain a URL", FieldPortion)
	}
	for _, f := range []struct {
		field string
		v     float64
		max   float64
	}{
		{FieldCalories, it.Calories, MaxItemCalories},
		{FieldProtein, it.ProteinG, MaxMacroGrams},
		{FieldCarbs, it.CarbsG, MaxMacroGrams},
		{FieldFat, it.FatG, MaxMacroGrams},
	} {
		if math.IsNaN(f.v) || math.IsInf(f.v, 0) || f.v < 0 || f.v > f.max {
			return fmt.Errorf("%s: %v out of range 0 to %v", f.field, f.v, f.max)
		}
	}
	return nil
}
