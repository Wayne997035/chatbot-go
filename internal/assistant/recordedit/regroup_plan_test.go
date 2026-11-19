package recordedit

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// [F34-10] 規格 #8：move_items / set_meal_type 的計畫驗證；任何一種不合法的都要 ErrInvalidPlan.
func TestParsePlanRegroupInvalid(t *testing.T) {
	manyItems := make([]string, 31)
	for i := range manyItems {
		manyItems[i] = fmt.Sprint(i + 1)
	}
	cases := []struct{ name, in string }{
		{"duplicate item across groups", `{"op":"move_items","target":"M1","moves":[{"items":[1],"meal_type":"breakfast"},{"items":[1,2],"meal_type":"lunch"}]}`},
		{"same meal_type in two groups", `{"op":"move_items","target":"M1","moves":[{"items":[1],"meal_type":"breakfast"},{"items":[2],"meal_type":"breakfast"}]}`},
		{"same meal_type differing only in case/space", `{"op":"move_items","target":"M1","moves":[{"items":[1],"meal_type":"lunch"},{"items":[2],"meal_type":" LUNCH "}]}`},
		{"unknown meal_type brunch in moves", `{"op":"move_items","target":"M1","moves":[{"items":[1],"meal_type":"brunch"}]}`},
		{"unknown meal_type in set_meal_type", `{"op":"set_meal_type","target":"M1","meal_type":"brunch"}`},
		{"chinese meal_type is not a code", `{"op":"set_meal_type","target":"M1","meal_type":"早午餐"}`},
		{"empty meal_type", `{"op":"set_meal_type","target":"M1","meal_type":""}`},
		{"missing meal_type", `{"op":"set_meal_type","target":"M1"}`},
		{"null meal_type", `{"op":"set_meal_type","target":"M1","meal_type":null}`},
		{"empty moves", `{"op":"move_items","target":"M1","moves":[]}`},
		{"missing moves", `{"op":"move_items","target":"M1"}`},
		{"null moves", `{"op":"move_items","target":"M1","moves":null}`},
		{"six groups", `{"op":"move_items","target":"M1","moves":[` + strings.Join([]string{
			`{"items":[1],"meal_type":"breakfast"}`, `{"items":[2],"meal_type":"lunch"}`, `{"items":[3],"meal_type":"snack"}`,
			`{"items":[4],"meal_type":"dinner"}`, `{"items":[5],"meal_type":"late_night"}`, `{"items":[6],"meal_type":"breakfast"}`,
		}, ",") + `]}`},
		{"group without items", `{"op":"move_items","target":"M1","moves":[{"items":[],"meal_type":"lunch"}]}`},
		{"group items 31", `{"op":"move_items","target":"M1","moves":[{"items":[` + strings.Join(manyItems, ",") + `],"meal_type":"lunch"}]}`},
		{"item index zero", `{"op":"move_items","target":"M1","moves":[{"items":[0],"meal_type":"lunch"}]}`},
		{"item index negative", `{"op":"move_items","target":"M1","moves":[{"items":[-1],"meal_type":"lunch"}]}`},
		{"duplicate item inside one group", `{"op":"move_items","target":"M1","moves":[{"items":[2,2],"meal_type":"lunch"}]}`},
		{"item index not integer", `{"op":"move_items","target":"M1","moves":[{"items":[1.5],"meal_type":"lunch"}]}`},
		{"moves wrong type", `{"op":"move_items","target":"M1","moves":"早餐"}`},
		{"move_items without target or candidates", `{"op":"move_items","moves":[{"items":[1],"meal_type":"lunch"}]}`},
		{"two targets", `{"op":"move_items","target":"M1,M2","moves":[{"items":[1],"meal_type":"lunch"}]}`},
	}
	for _, c := range cases {
		p, err := ParsePlan(c.in)
		if err == nil || !errors.Is(err, ErrInvalidPlan) || p != nil {
			t.Errorf("%s: plan=%+v err=%v, want ErrInvalidPlan", c.name, p, err)
		}
	}
}

// [F34-10] 合法的 regroup 計畫：餐別 trim＋小寫正規化、五個餐別全收、5 組上限剛好可用、候選路徑照舊.
func TestParsePlanRegroupValid(t *testing.T) {
	p, err := ParsePlan(`{"op":"set_meal_type","target":"M1","meal_type":" Breakfast "}`)
	if err != nil || p.Op != OpSetMealType || p.MealType != "breakfast" || p.Target != "M1" {
		t.Fatalf("set_meal_type: %+v %v", p, err)
	}
	for _, mt := range []string{"breakfast", "lunch", "snack", "dinner", "late_night"} {
		if p, err := ParsePlan(`{"op":"set_meal_type","target":"M2","meal_type":"` + mt + `"}`); err != nil || p.MealType != mt {
			t.Errorf("%s: %+v %v", mt, p, err)
		}
	}
	five := `{"op":"move_items","target":"M3","moves":[` + strings.Join([]string{
		`{"items":[1],"meal_type":"breakfast"}`, `{"items":[2],"meal_type":"lunch"}`, `{"items":[3],"meal_type":"SNACK"}`,
		`{"items":[4],"meal_type":"dinner"}`, `{"items":[5],"meal_type":"late_night"}`,
	}, ",") + `],"basis":"客人要分五餐"}`
	p, err = ParsePlan(five)
	if err != nil || p.Op != OpMoveItems || len(p.Moves) != 5 || p.Moves[2].MealType != "snack" || p.Moves[4].Items[0] != 5 {
		t.Fatalf("five groups: %+v %v", p, err)
	}
	p, err = ParsePlan(`{"op":"move_items","candidates":["M1","M2"],"moves":[{"items":[1,2],"meal_type":"lunch"}]}`)
	if err != nil || p.Target != "" || len(p.Candidates) != 2 {
		t.Fatalf("candidate path: %+v %v", p, err)
	}
	// 模型在非 regroup op 附了 meal_type / moves：不影響既有 op 的解析.
	if p, err := ParsePlan(`{"op":"set_item_kcal","target":"M1","item":1,"kcal":210,"meal_type":"junk"}`); err != nil || p.Op != OpSetItemKcal {
		t.Errorf("stray meal_type on another op: %+v %v", p, err)
	}
	// op=none 一律丟掉 regroup 欄位.
	if p, err := ParsePlan(`{"op":"none","meal_type":"lunch","moves":[{"items":[1],"meal_type":"lunch"}]}`); err != nil || p.Op != OpNone || p.MealType != "" || len(p.Moves) != 0 {
		t.Errorf("none keeps regroup fields: %+v %v", p, err)
	}
}
