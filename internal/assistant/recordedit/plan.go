package recordedit

import (
	"bytes"
	"chatbot-go/internal/nutrition"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 顯示字串上限（規格 §5：basis ≤80 字、品名 ≤30 字）.
const (
	MaxBasisRunes   = 80
	MaxNameRunes    = 30
	MaxPortionRunes = 30
	maxCandidates   = 5
	// [F34-10] move_items 一次最多幾組（五個餐別）、每組最多幾個品項序號.
	maxMoveGroups    = 5
	maxItemsPerGroup = 30
	maxPlanBytes     = 16 << 10 // AI 輸出超過這個長度的部分不看（避免對超長輸出逐個 '{' 重試）
)

var (
	// ErrNoChange 提議值與原值相同（呼叫端回「本來就是這樣」，不存暫存）.
	ErrNoChange = errors.New("recordedit: nothing to change")
	// ErrLastItem remove_item 只剩一個品項（要刪整餐請說刪掉這餐）.
	ErrLastItem = errors.New("recordedit: cannot remove the last item")
)

// targetRe 編號表代號只能是單一個英數 token；多個 target（「M1,M2」）或夾雜文字一律 Invalid.
var targetRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// validOps 封閉列舉：AI 輸出不在這份清單內一律 Invalid.
var validOps = map[string]bool{
	OpNone: true, OpSetTotalKcal: true, OpSetItemKcal: true, OpSetItemByWeight: true, OpScale: true,
	OpAddItem: true, OpRemoveItem: true, OpRenameItem: true, OpDeleteMeal: true, OpSetWorkout: true, OpDeleteWorkout: true,
	OpSetMealType: true, OpMoveItems: true,
}

// requiredKeys 各 op 必須出現（且不是 null）的欄位.
var requiredKeys = map[string][]string{
	OpSetTotalKcal:    {"kcal"},
	OpSetItemKcal:     {"item", "kcal"},
	OpSetItemByWeight: {"item", "grams", "kcal_per_100g"},
	OpScale:           {"factor"},
	OpAddItem:         {"new_item"},
	OpRemoveItem:      {"item"},
	OpRenameItem:      {"item"},
	OpSetMealType:     {"meal_type"},
	OpMoveItems:       {"moves"},
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidPlan, fmt.Sprintf(format, args...))
}

// ParsePlan [F33-14] 取第一個完整的 JSON 物件並驗證：op 在列舉內、必要欄位齊全、型別正確；op=none 回 (plan, nil).
// 不在 schema 內的欄位（含 userId）一律忽略；字串欄位已清理（見 SanitizeDisplay）.
func ParsePlan(text string) (*Plan, error) {
	if len(text) > maxPlanBytes {
		text = text[:maxPlanBytes]
	}
	raw, ok := firstJSONObject(text)
	if !ok {
		return nil, invalid("no json object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, invalid("not an object")
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalid("field type")
	}
	if !validOps[p.Op] {
		return nil, invalid("unknown op")
	}
	if p.Op == OpNone {
		// [F33-44] 合法的候選照留：op none 卻列了候選代表模型漏填 op，規劃器靠它判斷要不要重問；其餘欄位一律丟掉.
		none := &Plan{Op: OpNone}
		if checkCandidates(p.Candidates) == nil {
			none.Candidates = p.Candidates
		}
		return none, nil
	}
	if err := checkTarget(&p); err != nil {
		return nil, err
	}
	if err := checkRequired(&p, fields); err != nil {
		return nil, err
	}
	sanitizePlan(&p)
	return &p, nil
}

// checkRequired 各 op 的必要欄位（以原文有出現且不是 null 為準，避免 0 值被當成「有給」）.
func checkRequired(p *Plan, fields map[string]json.RawMessage) error {
	for _, k := range requiredKeys[p.Op] {
		if v, present := fields[k]; !present || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return invalid("missing %s", k)
		}
	}
	if p.Op == OpAddItem && (p.NewItem == nil || strings.TrimSpace(p.NewItem.Name) == "") {
		return invalid("new_item without name")
	}
	if p.Op == OpRenameItem && strings.TrimSpace(p.Name) == "" && strings.TrimSpace(p.Portion) == "" {
		return invalid("rename without name or portion")
	}
	switch p.Op {
	case OpSetMealType:
		return checkMealType(&p.MealType)
	case OpMoveItems:
		return checkMoves(p.Moves)
	}
	return nil
}

// checkMealType [F34-10] 餐別 trim＋小寫後必須是五個已知值之一（「早午餐」「brunch」一律 invalid）；通過時寫回正規化後的值.
func checkMealType(s *string) error {
	v := strings.ToLower(strings.TrimSpace(*s))
	if !nutrition.MealType(v).Valid() {
		return invalid("meal_type is not a known meal type")
	}
	*s = v
	return nil
}

// checkMoves [F34-10] moves 1–5 組；每組 items 非空、≤30 個、每個 ≥1；任兩組 items 不得重複序號、meal_type 不得相同.
// 序號上限（≤ 品項數）要知道紀錄才能檢查，由 BuildRegroupProposal 負責.
func checkMoves(moves []Move) error {
	if len(moves) < 1 || len(moves) > maxMoveGroups {
		return invalid("moves count")
	}
	seenItem := map[int]bool{}
	seenType := map[string]bool{}
	for i := range moves {
		mv := &moves[i]
		if len(mv.Items) < 1 || len(mv.Items) > maxItemsPerGroup {
			return invalid("move items count")
		}
		for _, n := range mv.Items {
			if n < 1 || seenItem[n] {
				return invalid("move item index")
			}
			seenItem[n] = true
		}
		if err := checkMealType(&mv.MealType); err != nil {
			return err
		}
		if seenType[mv.MealType] {
			return invalid("duplicate meal_type in moves")
		}
		seenType[mv.MealType] = true
	}
	return nil
}

// checkTarget target 只能是單一代號；缺 target 時一定要有 2–5 個候選（讓程式出候選按鈕）.
func checkTarget(p *Plan) error {
	if p.Target != "" && !targetRe.MatchString(p.Target) {
		return invalid("target is not a single code")
	}
	if len(p.Candidates) != 0 {
		if err := checkCandidates(p.Candidates); err != nil {
			return err
		}
	}
	if p.Target == "" && len(p.Candidates) == 0 {
		return invalid("missing target")
	}
	return nil
}

// checkCandidates 候選要 2–5 個、每個都是單一代號.
func checkCandidates(cands []string) error {
	if len(cands) < 2 || len(cands) > maxCandidates {
		return invalid("candidates count")
	}
	for _, c := range cands {
		if !targetRe.MatchString(c) {
			return invalid("candidate is not a single code")
		}
	}
	return nil
}

// firstJSONObject 依序試每個 '{'，回傳第一個能完整解出的 JSON 物件原文（前後的 prose 不理會）.
func firstJSONObject(text string) ([]byte, bool) {
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == nil && len(raw) > 0 && raw[0] == '{' {
			return raw, true
		}
	}
	return nil, false
}

// SanitizeDisplay [F33-14] 進回覆前的字串清理：去控制字元（換行、tab 變空白）、去 [[ 與 ]]（假冒記餐標記）、
// 壓縮空白、截到 maxRunes 個字（maxRunes ≤ 0 不截）.
func SanitizeDisplay(s string, maxRunes int) string {
	s = cleanText(s)
	if maxRunes > 0 && utf8.RuneCountInString(s) > maxRunes {
		s = strings.TrimSpace(string([]rune(s)[:maxRunes]))
	}
	return s
}

// cleanText 只清理不截斷.
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case unicode.IsControl(r) || r == ' ' || r == ' ':
			return -1
		}
		return r
	}, s)
	for strings.Contains(s, "[[") || strings.Contains(s, "]]") {
		s = strings.ReplaceAll(strings.ReplaceAll(s, "[[", ""), "]]", "")
	}
	return strings.Join(strings.Fields(s), " ")
}

func sanitizePlan(p *Plan) {
	p.Basis = SanitizeDisplay(p.Basis, MaxBasisRunes)
	p.Name = SanitizeDisplay(p.Name, MaxNameRunes)
	p.Portion = SanitizeDisplay(p.Portion, MaxPortionRunes)
	if p.NewItem != nil {
		// 新品項名稱不截斷：過長由 Build 拒絕（截斷會悄悄改掉客人看到的名稱）.
		p.NewItem.Name = cleanText(p.NewItem.Name)
		p.NewItem.Portion = SanitizeDisplay(p.NewItem.Portion, MaxPortionRunes)
	}
	p.Target = strings.TrimSpace(p.Target)
	p.DoneTime = strings.TrimSpace(p.DoneTime)
}
