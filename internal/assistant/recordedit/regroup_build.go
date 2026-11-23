package recordedit

import (
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/meal"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ErrSameMealType [F34-12] set_meal_type 的目的餐別與這筆本來的餐別相同.
var ErrSameMealType = errors.New("recordedit: already that meal type")

// IsRegroupOp [F34-12] 這個 op 是改餐別 / 移品項 / 拆多餐（呼叫端分流用；BuildMealProposal 對它們維持 invalid）.
func IsRegroupOp(op string) bool { return op == OpSetMealType || op == OpMoveItems }

// RegroupStep [F34-15] 一筆寫入的種類；ApplyRegroup 依 W → M → S 的順序回傳.
type RegroupStep int

// 寫入步驟（加法優先：先新增、再改併入目標、最後才動來源）.
const (
	StepInsert       RegroupStep = iota + 1 // W：新開一筆（Meal.ID 為空，Insert 補）
	StepUpdateMerge                         // M：併入既有的一筆（Meal 是要寫回的完整紀錄，Version 為最新讀出的值）
	StepUpdateSource                        // S：來源 Update（原地改或 remaining 非空）
	StepDeleteSource                        // S：來源 remaining 為空 → DeleteIfVersion
)

// RegroupWrite [F34-15] ApplyRegroup 算出的一筆寫入.
type RegroupWrite struct {
	Step RegroupStep
	Meal *meal.Meal // StepDeleteSource 為 nil
}

// regroupDayOffset [F34-12] 來源那天離今天幾天（≤0）；MealTime 的 dayOffset.
func regroupDayOffset(src *meal.Meal, now time.Time) int {
	nowDay, _ := nutrition.DayRange(now)
	srcDay, _ := nutrition.DayRange(src.EatenAt)
	days := int(nowDay.Sub(srcDay).Hours()/24 + 0.5)
	if days <= 0 {
		return 0
	}
	return -days
}

// regroupGroups [F34-12] 把計畫攤成分組（序號升冪）；set_meal_type 等同「全部品項移到一個餐別」.
func regroupGroups(plan *Plan, src *meal.Meal) ([]RegroupGroup, error) {
	n := len(src.Items)
	if n == 0 {
		return nil, invalid("meal has no items")
	}
	var groups []RegroupGroup
	switch plan.Op {
	case OpSetMealType:
		mt := strings.ToLower(strings.TrimSpace(plan.MealType))
		if !nutrition.MealType(mt).Valid() {
			return nil, invalid("meal_type is not a known meal type")
		}
		if src.MealType == mt {
			return nil, fmt.Errorf("%w: %s", ErrSameMealType, nutrition.MealType(mt).LabelAt(src.EatenAt)) // [F35-01] mt == src.MealType，名稱依這筆的時間
		}
		all := make([]int, n)
		for i := range all {
			all[i] = i + 1
		}
		groups = []RegroupGroup{{MealType: mt, Items: all}}
	case OpMoveItems:
		if len(plan.Moves) == 0 {
			return nil, invalid("no moves")
		}
		for i := range plan.Moves {
			mv := &plan.Moves[i]
			mt := strings.ToLower(strings.TrimSpace(mv.MealType))
			if !nutrition.MealType(mt).Valid() {
				return nil, invalid("meal_type is not a known meal type")
			}
			items := slices.Clone(mv.Items)
			slices.Sort(items)
			for _, it := range items {
				if it < 1 || it > n {
					return nil, outOfRange("沒有第 %d 個品項", it)
				}
			}
			groups = append(groups, RegroupGroup{MealType: mt, Items: items, Stay: mt == src.MealType})
		}
	default:
		return nil, invalid("op %q is not a regroup", plan.Op)
	}
	return groups, nil
}

// remainingItems [F34-12] 沒被移走的品項序號：沒被列到的 ＋ stay 組的，保持原順序.
func remainingItems(n int, groups []RegroupGroup) []int {
	moved := map[int]bool{}
	for i := range groups {
		if groups[i].Stay {
			continue
		}
		for _, it := range groups[i].Items {
			moved[it] = true
		}
	}
	var rest []int
	for i := 1; i <= n; i++ {
		if !moved[i] {
			rest = append(rest, i)
		}
	}
	return rest
}

// pickMergeTarget [F34-12] 同日同餐別的併入目標：ID != 來源、餐別相同、屬於同一個使用者；多筆取 EatenAt 最早，同時間取 ID 小者.
// 合併目標一律由程式從 sameDay 選，AI 不能指定.
func pickMergeTarget(sameDay []meal.Meal, src *meal.Meal, mealType string) *meal.Meal {
	var best *meal.Meal
	for i := range sameDay {
		c := &sameDay[i]
		if c.ID == src.ID || c.UserID != src.UserID || c.MealType != mealType {
			continue
		}
		if best == nil || c.EatenAt.Before(best.EatenAt) || (c.EatenAt.Equal(best.EatenAt) && c.ID < best.ID) {
			best = c
		}
	}
	return best
}

// BuildRegroupProposal [F34-12] 改餐別 / 移品項 / 拆多餐的試算（純函式，入參不被修改）.
// sameDay 是這位使用者在來源那天的全部餐點（不能只用編號表的 12 筆，併入目標可能不在表內）.
// 數字全由程式算；EatenAt 與併入目標的 ID / Version 在提議當下固定存進 Proposal.Regroup，確認時不重算.
func BuildRegroupProposal(plan *Plan, src *meal.Meal, sameDay []meal.Meal, now time.Time) (*Proposal, error) {
	if plan == nil || src == nil || !IsRegroupOp(plan.Op) {
		return nil, invalid("not a regroup plan")
	}
	groups, err := regroupGroups(plan, src)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(groups, func(g RegroupGroup) bool { return !g.Stay }) {
		return nil, ErrNoChange
	}
	merges := map[string]*meal.Meal{}
	dayOff := regroupDayOffset(src, now)
	before := src.Total.Calories
	for i := range groups {
		g := &groups[i]
		if g.Stay {
			continue
		}
		if t := pickMergeTarget(sameDay, src, g.MealType); t != nil {
			g.MergeID, g.MergeBase = t.ID, t.Version
			merges[t.ID] = t
			before += t.Total.Calories
			continue
		}
		g.EatenAt = nutrition.MealTime(now, nutrition.MealType(g.MealType), dayOff)
	}
	spec := &RegroupSpec{SourceID: src.ID, SourceBase: src.Version, Groups: groups}
	if rest := remainingItems(len(src.Items), groups); len(rest) == 0 {
		if only := onlyNonStay(groups); only != nil && only.MergeID == "" {
			spec.InPlace = true
		} else {
			spec.Emptied = true
		}
	}
	writes, err := ApplyRegroup(spec, src, merges)
	if err != nil {
		return nil, err
	}
	after := 0.0
	for i := range writes {
		if writes[i].Meal != nil {
			after += writes[i].Meal.Total.Calories
		}
	}
	return &Proposal{
		Kind: KindMeal, Op: plan.Op, RecordID: src.ID, BaseVersion: src.Version, Plan: *plan,
		Before: before, After: after, Regroup: spec,
		Summary: regroupSummary(plan, src, spec, merges, writes),
	}, nil
}

// onlyNonStay [F34-12] 恰有一組非 stay 時回那一組，否則 nil.
func onlyNonStay(groups []RegroupGroup) *RegroupGroup {
	var found *RegroupGroup
	for i := range groups {
		if groups[i].Stay {
			continue
		}
		if found != nil {
			return nil
		}
		found = &groups[i]
	}
	return found
}

// ApplyRegroup [F34-12] 依 spec 在「最新讀出的」來源與併入目標上重算要寫的紀錄（與 BuildRegroupProposal 共用同一份分組邏輯）.
// 回傳順序固定為 W（新開）→ M（併入）→ S（來源最後）；入參不被修改；序號超出品項數、併入後超過整餐上限回 ErrOutOfRange.
// 新開紀錄的 CreateTime 留空，由 Insert 補現在. 紀錄是否被改過（Version）由呼叫端先比對 spec 裡的 base.
func ApplyRegroup(spec *RegroupSpec, src *meal.Meal, targets map[string]*meal.Meal) ([]RegroupWrite, error) {
	if spec == nil || src == nil || src.ID != spec.SourceID || len(spec.Groups) == 0 {
		return nil, invalid("bad regroup spec")
	}
	if err := checkSpecItems(spec, len(src.Items)); err != nil {
		return nil, err
	}
	var inserts, merges []RegroupWrite
	if !spec.InPlace { // 原地改不搬品項
		for i := range spec.Groups {
			g := &spec.Groups[i]
			if g.Stay {
				continue
			}
			w, err := groupWrite(g, src, targets)
			if err != nil {
				return nil, err
			}
			if w.Step == StepInsert {
				inserts = append(inserts, w)
			} else {
				merges = append(merges, w)
			}
		}
	}
	sw, err := sourceWrite(spec, src)
	if err != nil {
		return nil, err
	}
	return slices.Concat(inserts, merges, []RegroupWrite{sw}), nil
}

// checkSpecItems [F34-12] 每個序號都在 1..n 內、沒有一個序號被列進兩組；
// 原地改與刪來源都只在「沒有任何品項留下」時成立——spec 與紀錄對不上就拒絕，不能讓來源在還有品項時被刪或被原地改.
func checkSpecItems(spec *RegroupSpec, n int) error {
	listed := map[int]bool{}
	for i := range spec.Groups {
		for _, it := range spec.Groups[i].Items {
			if it < 1 || it > n {
				return outOfRange("沒有第 %d 個品項", it)
			}
			if listed[it] {
				return invalid("item listed in two groups")
			}
			listed[it] = true
		}
	}
	if (spec.InPlace || spec.Emptied) && len(remainingItems(n, spec.Groups)) != 0 {
		return invalid("spec expects an empty source")
	}
	return nil
}

// groupWrite [F34-12] 一個非 stay 組的寫入：有併入目標 → 併入（Rebase+Recalc、檢查整餐上限）；沒有 → 新開一筆.
func groupWrite(g *RegroupGroup, src *meal.Meal, targets map[string]*meal.Meal) (RegroupWrite, error) {
	moved := make([]meal.FoodItem, 0, len(g.Items))
	for _, it := range g.Items {
		moved = append(moved, copyItem(&src.Items[it-1]))
	}
	if g.MergeID != "" {
		t := targets[g.MergeID]
		if t == nil || t.UserID != src.UserID || t.MealType != g.MealType {
			return RegroupWrite{}, invalid("merge target missing")
		}
		w := copyMeal(t)
		w.Items = append(w.Items, moved...)
		w.Rebase()
		if w.Total.Calories > maxTotalKcal {
			return RegroupWrite{}, outOfRange("整餐熱量不可超過 %d 大卡", maxTotalKcal)
		}
		return RegroupWrite{Step: StepUpdateMerge, Meal: w}, nil
	}
	nw := &meal.Meal{
		UserID: src.UserID, EatenAt: g.EatenAt, MealType: g.MealType, Items: moved,
		Source: src.Source, Note: src.Note,
	}
	if src.Place != nil {
		place := *src.Place
		nw.Place = &place
	}
	nw.Rebase()
	return RegroupWrite{Step: StepInsert, Meal: nw}, nil
}

// sourceWrite [F34-12] 來源最後處理：原地改（只改餐別與時間，其餘位元不變）、刪除（remaining 為空）、或瘦身 Update（Rebase+Recalc）.
func sourceWrite(spec *RegroupSpec, src *meal.Meal) (RegroupWrite, error) {
	switch {
	case spec.InPlace:
		g := onlyNonStay(spec.Groups)
		if g == nil {
			return RegroupWrite{}, invalid("in-place regroup needs exactly one moving group")
		}
		w := copyMeal(src) // Items / Total / Scale / Base 位元不變，保留舊資料的合計
		w.MealType, w.EatenAt = g.MealType, g.EatenAt
		return RegroupWrite{Step: StepUpdateSource, Meal: w}, nil
	case spec.Emptied:
		return RegroupWrite{Step: StepDeleteSource}, nil
	}
	rest := remainingItems(len(src.Items), spec.Groups)
	if len(rest) == 0 {
		return RegroupWrite{}, invalid("source would be empty but spec does not delete it")
	}
	w := copyMeal(src)
	w.Items = make([]meal.FoodItem, 0, len(rest))
	for _, it := range rest {
		w.Items = append(w.Items, src.Items[it-1])
	}
	w.Rebase()
	return RegroupWrite{Step: StepUpdateSource, Meal: w}, nil
}

// CloneMeal [F34-15] 深拷貝餐點（品項與其 Base）；確認寫入前抓快照、撤回時寫回用.
func CloneMeal(m *meal.Meal) *meal.Meal { return copyMeal(m) }

// copyItem 深拷貝一個品項（含 Base）.
func copyItem(f *meal.FoodItem) meal.FoodItem {
	c := *f
	if f.Base != nil {
		b := *f.Base
		c.Base = &b
	}
	return c
}

// groupNames [F34-13] 這組的品名以「、」串接（已清理並截斷）.
func groupNames(src *meal.Meal, items []int) string {
	names := make([]string, 0, len(items))
	for _, it := range items {
		if it >= 1 && it <= len(src.Items) {
			names = append(names, src.Items[it-1].Name)
		}
	}
	return SanitizeDisplay(strings.Join(names, "、"), MaxNameRunes)
}

// regroupSummary [F34-13] 確認卡文字（程式樣板，無 AI 自由句；每個受影響的紀錄一行，數字取自試算）.
// 每行長度有上限（品名 ≤30 字、依據 ≤80 字、至多 5 組加來源一行），總長遠小於確認卡上限.
func regroupSummary(plan *Plan, src *meal.Meal, spec *RegroupSpec, merges map[string]*meal.Meal, writes []RegroupWrite) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "要這樣調整 %s 的餐點嗎？", src.EatenAt.In(nutrition.Taipei).Format("01/02"))
	srcLabel := nutrition.MealType(src.MealType).LabelAt(src.EatenAt) // [F35-01] 來源名稱依來源自己的時間
	// 寫入結果依 W、M 順序對應到非 stay 的組：先新開、再併入（與 ApplyRegroup 的順序一致）.
	totals := map[int]float64{}
	var newIdx, mergeIdx []int
	for i := range spec.Groups {
		switch g := &spec.Groups[i]; {
		case g.Stay || spec.InPlace:
		case g.MergeID != "":
			mergeIdx = append(mergeIdx, i)
		default:
			newIdx = append(newIdx, i)
		}
	}
	w := 0
	for _, i := range append(slices.Clone(newIdx), mergeIdx...) {
		totals[i] = writes[w].Meal.Total.Calories
		w++
	}
	for i := range spec.Groups {
		g := &spec.Groups[i]
		if g.Stay {
			continue
		}
		// [F35-01] 名稱依這組實際寫入的 EatenAt；併入時保留目標那筆的時間，g.EatenAt 對併入組是零值.
		at := g.EatenAt
		if g.MergeID != "" {
			at = merges[g.MergeID].EatenAt
		}
		label := nutrition.MealType(g.MealType).LabelAt(at)
		switch {
		case spec.InPlace:
			fmt.Fprintf(&sb, "\n・%s：改為%s（%.0f 大卡不變）", srcLabel, label, src.Total.Calories)
		case g.MergeID != "":
			fmt.Fprintf(&sb, "\n・%s：「%s」→ 併入原有 %.0f → %.0f 大卡", label, groupNames(src, g.Items), merges[g.MergeID].Total.Calories, totals[i])
		default:
			fmt.Fprintf(&sb, "\n・%s：「%s」→ 新增 %.0f 大卡", label, groupNames(src, g.Items), totals[i])
		}
	}
	switch {
	case spec.InPlace:
	case spec.Emptied:
		fmt.Fprintf(&sb, "\n・%s：整筆移走，這筆會刪除（原 %.0f）", srcLabel, src.Total.Calories)
	default:
		fmt.Fprintf(&sb, "\n・%s：剩 %.0f 大卡（原 %.0f）", srcLabel, writes[len(writes)-1].Meal.Total.Calories, src.Total.Calories)
	}
	sb.WriteString(basisLine(plan.Basis))
	return sb.String()
}
