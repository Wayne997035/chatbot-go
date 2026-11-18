package webhook

import (
	"chatbot-go/internal/assistant/recordedit"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// [F34-12][F34-15] 改餐別 / 移品項 / 拆多餐的 webhook 測試.
// 固定資料 F（規格驗收段）：U1 今天晚餐 18:30、五項 450/200/300/500/360，合計 1810；預期值皆由品項熱量相加推得.
// 時鐘（dispatch「時鐘」條）：精確 EatenAt 的測試由 f.planner.fn 用 recordedit.BuildRegroupProposal 加上「台北今天 20:00」自建提議，
// 並用 f.setNow 把 h.clock 設成同一時刻；餐點的 EatenAt 取真實今天（todayTotalLine 用 time.Now() 算今日，見 food_handler.go）.

const regroupUser = "U1"

// rgToday 台北真實今天的某個時刻.
func rgToday(h, m int) time.Time {
	n := time.Now().In(nutrition.Taipei)
	return time.Date(n.Year(), n.Month(), n.Day(), h, m, 0, 0, nutrition.Taipei)
}

// rgNow 提議當下的時鐘：台北今天 20:00（早餐 08:00、午餐 12:30、點心 15:30 都已過，nutrition.MealTime 會放在典型時間）.
func rgNow() time.Time { return rgToday(20, 0) }

// seedF 固定資料 F.
func (f *editFixture) seedF(t *testing.T, userID string) *meal.Meal {
	t.Helper()
	names := []string{"牛肉火腿起司三明治", "沙拉", "雞胸肉", "飯", "湯"}
	kcal := []float64{450, 200, 300, 500, 360}
	m := &meal.Meal{UserID: userID, EatenAt: rgToday(18, 30), CreateTime: rgToday(18, 30), MealType: "dinner", Source: meal.SourceText, Note: "晚餐吃了很多"}
	for i := range names {
		m.Items = append(m.Items, meal.FoodItem{Name: names[i], Portion: "1份", Nutrients: meal.Nutrients{Calories: kcal[i], ProteinG: 5}})
	}
	m.Recalc()
	if err := f.meals.Insert(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m
}

// seedSlot 當天另一筆單品項餐點（有餐別）.
func (f *editFixture) seedSlot(t *testing.T, userID, mealType, name string, at time.Time, kcal float64) *meal.Meal {
	t.Helper()
	m := &meal.Meal{
		UserID: userID, EatenAt: at, CreateTime: at, MealType: mealType, Source: meal.SourceText,
		Items: []meal.FoodItem{{Name: name, Portion: "1份", Nutrients: meal.Nutrients{Calories: kcal}}},
	}
	m.Recalc()
	if err := f.meals.Insert(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m
}

// override 讓假 repo 的某次呼叫回指定結果（nil = 照常交給真的記憶體實作）.
type override struct {
	ok  bool
	err error
}

// faultMeals 計數並可依「第 N 次呼叫」注入結果的餐點儲存；hook 在 mu 之外呼叫，可安全回頭改底層資料.
type faultMeals struct {
	meal.MealRepository
	mu                                       sync.Mutex
	inserts, updates, deletes, deleteIfs, fi int
	insertHook                               func(n int, m *meal.Meal) error
	afterInsert                              func(n int)
	updateHook                               func(n int, m *meal.Meal) *override
	deleteHook                               func(n int, id string) *override
	deleteIfHook                             func(n int) *override
	findHook                                 func(n int, id string) (*meal.Meal, bool)
}

func (f *faultMeals) Insert(ctx context.Context, m *meal.Meal) error {
	f.mu.Lock()
	f.inserts++
	n := f.inserts
	f.mu.Unlock()
	if f.insertHook != nil {
		if err := f.insertHook(n, m); err != nil {
			return err
		}
	}
	err := f.MealRepository.Insert(ctx, m)
	if err == nil && f.afterInsert != nil {
		f.afterInsert(n)
	}
	return err
}

func (f *faultMeals) Update(ctx context.Context, m *meal.Meal) (bool, error) {
	f.mu.Lock()
	f.updates++
	n := f.updates
	f.mu.Unlock()
	if f.updateHook != nil {
		if o := f.updateHook(n, m); o != nil {
			return o.ok, o.err
		}
	}
	return f.MealRepository.Update(ctx, m)
}

func (f *faultMeals) DeleteByID(ctx context.Context, userID, id string) (bool, error) {
	f.mu.Lock()
	f.deletes++
	n := f.deletes
	f.mu.Unlock()
	if f.deleteHook != nil {
		if o := f.deleteHook(n, id); o != nil {
			return o.ok, o.err
		}
	}
	return f.MealRepository.DeleteByID(ctx, userID, id)
}

func (f *faultMeals) DeleteIfVersion(ctx context.Context, userID, id string, version int64) (bool, error) {
	f.mu.Lock()
	f.deleteIfs++
	n := f.deleteIfs
	f.mu.Unlock()
	if f.deleteIfHook != nil {
		if o := f.deleteIfHook(n); o != nil {
			return o.ok, o.err
		}
	}
	return f.MealRepository.DeleteIfVersion(ctx, userID, id, version)
}

func (f *faultMeals) FindByID(ctx context.Context, userID, id string) (*meal.Meal, error) {
	f.mu.Lock()
	f.fi++
	n := f.fi
	f.mu.Unlock()
	if f.findHook != nil {
		if m, handled := f.findHook(n, id); handled {
			return m, nil
		}
	}
	return f.MealRepository.FindByID(ctx, userID, id)
}

// counts 寫入類呼叫次數（Insert / Update / DeleteByID / DeleteIfVersion）.
func (f *faultMeals) counts() [4]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return [4]int{f.inserts, f.updates, f.deletes, f.deleteIfs}
}

// replyText 最後一則回覆的文字本身（不是 JSON，換行保留）.
func replyText(t *testing.T, f *editFixture) string {
	t.Helper()
	if len(f.api.replies) == 0 {
		t.Fatal("no reply sent")
	}
	msgs, _ := f.api.replies[len(f.api.replies)-1]["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("reply has no messages")
	}
	text, _ := msgs[0].(map[string]any)["text"].(string)
	return text
}

// dbShape U1 全部餐點的「餐別:合計/品項數」，排序後以逗號串接（比對最終 DB 內容用）.
func dbShape(f *editFixture) string {
	var parts []string
	for i := range f.meals.Items {
		if m := &f.meals.Items[i]; m.UserID == regroupUser {
			parts = append(parts, fmt.Sprintf("%s:%.0f/%d", m.MealType, m.Total.Calories, len(m.Items)))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (f *editFixture) mealByID(id string) *meal.Meal {
	for i := range f.meals.Items {
		if f.meals.Items[i].ID == id {
			m := f.meals.Items[i]
			return &m
		}
	}
	return nil
}

func (f *editFixture) mealsOfType(mealType string) []meal.Meal {
	var out []meal.Meal
	for i := range f.meals.Items {
		if m := f.meals.Items[i]; m.UserID == regroupUser && m.MealType == mealType {
			out = append(out, m)
		}
	}
	return out
}

// offerRegroup 讓 U 提出 regroup 計畫：提議由 recordedit.BuildRegroupProposal 在 rgNow() 自建（見檔頭「時鐘」），回傳確認鈕 data.
func (f *editFixture) offerRegroup(t *testing.T, userID, srcID, planJSON string) string {
	t.Helper()
	f.setNow(rgNow())
	f.planner.fn = func(ctx context.Context, uid, _ string) (*recordedit.Outcome, error) {
		plan, err := recordedit.ParsePlan(planJSON)
		if err != nil {
			return nil, err
		}
		src, err := f.meals.FindByID(ctx, uid, srcID)
		if err != nil || src == nil {
			return nil, fmt.Errorf("source %q missing: %w", srcID, err)
		}
		from, to := nutrition.DayRange(src.EatenAt)
		sameDay, err := f.meals.FindByUserBetween(ctx, uid, from, to)
		if err != nil {
			return nil, err
		}
		prop, err := recordedit.BuildRegroupProposal(plan, src, sameDay, rgNow())
		if err != nil {
			return nil, err
		}
		return &recordedit.Outcome{Kind: recordedit.OutcomeProposal, Proposal: prop}, nil
	}
	f.sayAs(t, userID, "幫我分成早午晚三餐")
	ok := f.buttonData(t, "ok")
	if ok == "" {
		t.Fatalf("no confirm button in %s", f.api.lastReply(t))
	}
	return ok
}

// confirmRegroupVia 以按鈕確認，並在確認前換上計數 / 注入用的儲存；回傳那個儲存與回覆文字.
func (f *editFixture) tapWith(t *testing.T, ok string, setup func(fm *faultMeals)) (fm *faultMeals, reply string) {
	t.Helper()
	fm = &faultMeals{MealRepository: f.meals}
	if setup != nil {
		setup(fm)
	}
	f.h.mealRepo = fm
	f.tapReply(t, regroupUser, ok)
	return fm, replyText(t, f)
}

func rgDate() string { return rgToday(18, 30).Format("01/02") }

const (
	planBreakfastWhole = `{"op":"set_meal_type","target":"M1","meal_type":"breakfast"}`
	planOneToBreakfast = `{"op":"move_items","target":"M1","moves":[{"items":[1],"meal_type":"breakfast"}]}`
	planThreeDinner    = `{"op":"move_items","target":"M1","moves":[{"items":[1],"meal_type":"breakfast"},{"items":[2,3],"meal_type":"lunch"},{"items":[4,5],"meal_type":"dinner"}]}`
	planThreeSnack     = `{"op":"move_items","target":"M1","moves":[{"items":[1],"meal_type":"breakfast"},{"items":[2,3],"meal_type":"lunch"},{"items":[4,5],"meal_type":"snack"}]}`
	planChickenLunch   = `{"op":"move_items","target":"M1","moves":[{"items":[3],"meal_type":"lunch"}]}`
)

// 規格 #1：整筆改餐別、當天沒有該餐別 → 原地改（同一筆、ID 不變、合計不變、Version+1），不 Insert / Delete.
func TestRegroupWholeMealInPlace(t *testing.T) {
	f := newEditFixture(t)
	src := f.seedF(t, regroupUser)
	ok := f.offerRegroup(t, regroupUser, src.ID, planBreakfastWhole)
	mustContain(t, replyText(t, f), "要這樣調整 "+rgDate()+" 的餐點嗎？", "・晚餐：改為早餐（1810 大卡不變）")
	fm, reply := f.tapWith(t, ok, nil)

	if got := dbShape(f); got != "breakfast:1810/5" {
		t.Fatalf("db = %s, want a single breakfast of 1810 with the same 5 items", got)
	}
	m := f.mealByID(src.ID)
	if m == nil || m.Version != src.Version+1 || !m.EatenAt.Equal(rgToday(8, 0)) || m.Total.Calories != 1810 {
		t.Errorf("record = %+v, want same ID, version %d, eaten at 08:00, total 1810", m, src.Version+1)
	}
	if c := fm.counts(); c != [4]int{0, 1, 0, 0} {
		t.Errorf("insert/update/delete/deleteIf = %v, want 0/1/0/0", c)
	}
	want := "已調整 " + rgDate() + " 的餐點：\n・早餐 1810 大卡（原為晚餐）\n今日累計：1810 大卡（1 餐）"
	if reply != want {
		t.Errorf("reply:\n%s\nwant:\n%s", reply, want)
	}
}

// 規格 #2：移一個品項到沒有的餐別 → 新增一筆、來源瘦身；兩筆合計 1810；今日累計 2 餐.
func TestRegroupMoveOneItemToNewMeal(t *testing.T) {
	f := newEditFixture(t)
	src := f.seedF(t, regroupUser)
	ok := f.offerRegroup(t, regroupUser, src.ID, planOneToBreakfast)
	mustContain(t, replyText(t, f), "・早餐：「牛肉火腿起司三明治」→ 新增 450 大卡", "・晚餐：剩 1360 大卡（原 1810）")
	fm, reply := f.tapWith(t, ok, nil)

	if got := dbShape(f); got != "breakfast:450/1,dinner:1360/4" {
		t.Fatalf("db = %s", got)
	}
	if b := f.mealsOfType("breakfast"); len(b) != 1 || !b[0].EatenAt.Equal(rgToday(8, 0)) || b[0].Version != 0 || b[0].UserID != regroupUser {
		t.Errorf("new breakfast = %+v, want eaten at 08:00 owned by U1", b)
	}
	if c := fm.counts(); c != [4]int{1, 1, 0, 0} {
		t.Errorf("insert/update/delete/deleteIf = %v, want 1/1/0/0", c)
	}
	want := "已調整 " + rgDate() + " 的餐點：\n・早餐 450 大卡（新增）\n・晚餐 1360 大卡（原 1810）\n今日累計：1810 大卡（2 餐）"
	if reply != want {
		t.Errorf("reply:\n%s\nwant:\n%s", reply, want)
	}
}

// 規格 #3：當天已有午餐 600 → 併入（不新開）：午餐 900、晚餐 1510；餐數不變.
func TestRegroupMergeIntoExistingMeal(t *testing.T) {
	f := newEditFixture(t)
	src := f.seedF(t, regroupUser)
	lunch := f.seedSlot(t, regroupUser, "lunch", "既有午餐便當", rgToday(12, 0), 600)
	ok := f.offerRegroup(t, regroupUser, src.ID, planChickenLunch)
	mustContain(t, replyText(t, f), "・午餐：「雞胸肉」→ 併入原有 600 → 900 大卡", "・晚餐：剩 1510 大卡（原 1810）")
	fm, reply := f.tapWith(t, ok, nil)

	if got := dbShape(f); got != "dinner:1510/4,lunch:900/2" {
		t.Fatalf("db = %s, want lunch 900 (merged) and dinner 1510, no third record", got)
	}
	if m := f.mealByID(lunch.ID); m == nil || m.Version != lunch.Version+1 {
		t.Errorf("merged lunch = %+v, want same ID and version+1", m)
	}
	if c := fm.counts(); c != [4]int{0, 2, 0, 0} {
		t.Errorf("insert/update/delete/deleteIf = %v, want 0/2/0/0", c)
	}
	want := "已調整 " + rgDate() + " 的餐點：\n・午餐 900 大卡\n・晚餐 1510 大卡（原 1810）\n今日累計：2410 大卡（2 餐）"
	if reply != want {
		t.Errorf("reply:\n%s\nwant:\n%s", reply, want)
	}
}

// 規格 #4：拆三餐、晚餐那組是 stay → 不新開第二筆晚餐；三筆合計 1810.
func TestRegroupSplitThreeMealsDinnerStays(t *testing.T) {
	f := newEditFixture(t)
	src := f.seedF(t, regroupUser)
	ok := f.offerRegroup(t, regroupUser, src.ID, planThreeDinner)
	card := replyText(t, f)
	mustContain(t, card, "・早餐：「牛肉火腿起司三明治」→ 新增 450 大卡", "・午餐：「沙拉、雞胸肉」→ 新增 500 大卡", "・晚餐：剩 860 大卡（原 1810）")
	fm, reply := f.tapWith(t, ok, nil)

	if got := dbShape(f); got != "breakfast:450/1,dinner:860/2,lunch:500/2" {
		t.Fatalf("db = %s, want exactly one dinner of 860", got)
	}
	if n := len(f.mealsOfType("dinner")); n != 1 {
		t.Errorf("%d dinners, a stay group must not create a second one", n)
	}
	if c := fm.counts(); c != [4]int{2, 1, 0, 0} {
		t.Errorf("insert/update/delete/deleteIf = %v, want 2/1/0/0", c)
	}
	if !strings.HasSuffix(reply, "\n今日累計：1810 大卡（3 餐）") {
		t.Errorf("reply = %q, want the 1810 / 3 meals line last", reply)
	}
	mustContain(t, reply, "・早餐 450 大卡（新增）", "・午餐 500 大卡（新增）", "・晚餐 860 大卡（原 1810）")
}

// 規格 #5：拆三餐、來源清空 → 來源被刪（DeleteIfVersion 1 次）；早餐 450、午餐 500、點心 860.
func TestRegroupSplitDeletesEmptiedSource(t *testing.T) {
	f := newEditFixture(t)
	src := f.seedF(t, regroupUser)
	ok := f.offerRegroup(t, regroupUser, src.ID, planThreeSnack)
	mustContain(t, replyText(t, f), "・點心：「飯、湯」→ 新增 860 大卡", "・晚餐：整筆移走，這筆會刪除（原 1810）")
	fm, reply := f.tapWith(t, ok, nil)

	if got := dbShape(f); got != "breakfast:450/1,lunch:500/2,snack:860/2" {
		t.Fatalf("db = %s", got)
	}
	if f.mealByID(src.ID) != nil {
		t.Error("the emptied source is still there")
	}
	if c := fm.counts(); c != [4]int{3, 0, 0, 1} {
		t.Errorf("insert/update/delete/deleteIf = %v, want 3/0/0/1", c)
	}
	if sn := f.mealsOfType("snack"); len(sn) != 1 || !sn[0].EatenAt.Equal(rgToday(15, 30)) {
		t.Errorf("snack = %+v, want eaten at 15:30", sn)
	}
	mustContain(t, reply, "・晚餐 這筆已刪除（原 1810）")
	if !strings.HasSuffix(reply, "\n今日累計：1810 大卡（3 餐）") {
		t.Errorf("reply = %q", reply)
	}
}

// 規格 #25：拆分之後「今日累計」的餐數與熱量照寫入後的資料算（1 餐 → 3 餐）.
func TestRegroupTodayTotalAfterSplit(t *testing.T) {
	f := newEditFixture(t)
	src := f.seedF(t, regroupUser)
	ok := f.offerRegroup(t, regroupUser, src.ID, planThreeSnack)
	_, reply := f.tapWith(t, ok, nil)
	lines := strings.Split(reply, "\n")
	if last := lines[len(lines)-1]; last != "今日累計：1810 大卡（3 餐）" {
		t.Errorf("last line = %q, want exactly 今日累計：1810 大卡（3 餐）", last)
	}
}

// 規格 #9：單品項來源移走唯一品項：(a) 目的餐別當天沒有 → 原地改；(b) 已有 → 併入、來源刪除（餐數 −1）.
func TestRegroupSingleItemSource(t *testing.T) {
	plan := `{"op":"move_items","target":"M1","moves":[{"items":[1],"meal_type":"dinner"}]}`
	t.Run("a: no dinner yet, changed in place", func(t *testing.T) {
		f := newEditFixture(t)
		x := f.seedSlot(t, regroupUser, "lunch", "排骨便當", rgToday(12, 30), 300)
		ok := f.offerRegroup(t, regroupUser, x.ID, plan)
		fm, reply := f.tapWith(t, ok, nil)
		if got := dbShape(f); got != "dinner:300/1" {
			t.Fatalf("db = %s", got)
		}
		if m := f.mealByID(x.ID); m == nil || !m.EatenAt.Equal(rgToday(18, 30)) {
			t.Errorf("record = %+v, want the same ID eaten at 18:30", m)
		}
		if c := fm.counts(); c != [4]int{0, 1, 0, 0} {
			t.Errorf("insert/update/delete/deleteIf = %v, want 0/1/0/0", c)
		}
		if !strings.HasSuffix(reply, "\n今日累計：300 大卡（1 餐）") {
			t.Errorf("reply = %q, meal count must stay 1", reply)
		}
	})
	t.Run("b: dinner exists, merged and the emptied source is deleted", func(t *testing.T) {
		f := newEditFixture(t)
		x := f.seedSlot(t, regroupUser, "lunch", "排骨便當", rgToday(12, 30), 300)
		f.seedSlot(t, regroupUser, "dinner", "既有晚餐", rgToday(18, 0), 500)
		ok := f.offerRegroup(t, regroupUser, x.ID, plan)
		fm, reply := f.tapWith(t, ok, nil)
		if got := dbShape(f); got != "dinner:800/2" {
			t.Fatalf("db = %s, want dinner 800 and no leftover empty source", got)
		}
		if f.mealByID(x.ID) != nil {
			t.Error("the source is still there")
		}
		if c := fm.counts(); c != [4]int{0, 1, 0, 1} {
			t.Errorf("insert/update/delete/deleteIf = %v, want 0/1/0/1", c)
		}
		if !strings.HasSuffix(reply, "\n今日累計：800 大卡（1 餐）") {
			t.Errorf("reply = %q, meal count must drop from 2 to 1", reply)
		}
	})
}

// 規格 #6：整筆改成本來的餐別 → 不出卡、不 Put，回「這餐本來就是晚餐，不用改」（走真的規劃器）.
func TestRegroupSameMealTypeNoCard(t *testing.T) {
	f := newEditFixture(t)
	f.seedF(t, regroupUser)
	f.plan.answers = []string{`{"op":"set_meal_type","target":"M1","meal_type":"dinner"}`}
	reply := f.sayAs(t, regroupUser, "這筆改成晚餐")
	mustContain(t, reply, "這餐本來就是晚餐，不用改")
	if strings.Contains(reply, editNoChangeMessage) {
		t.Errorf("must not say the number is unchanged: %s", reply)
	}
	if f.store.Puts.Load() != 0 || f.buttonData(t, "ok") != "" {
		t.Error("a card was offered for a no-op")
	}
	f.assertNothingWritten(t, 1, 0)
}

// 規格 #7：序號超出品項數 → 不出卡、不 Put、說明範圍.
func TestRegroupItemOutOfRangeNoCard(t *testing.T) {
	f := newEditFixture(t)
	f.seedF(t, regroupUser)
	f.plan.answers = []string{`{"op":"move_items","target":"M1","moves":[{"items":[6],"meal_type":"breakfast"}]}`}
	reply := f.sayAs(t, regroupUser, "把第六項移到早餐")
	mustContain(t, reply, "沒有第 6 個品項")
	if f.store.Puts.Load() != 0 || f.buttonData(t, "ok") != "" {
		t.Error("a card was offered for an out-of-range item")
	}
	f.assertNothingWritten(t, 1, 0)
}

// 規格 #8：不合法的計畫 → editInvalidMessage、不出卡.
func TestRegroupInvalidPlanReply(t *testing.T) {
	bad := map[string]string{
		"same item in two groups": `{"op":"move_items","target":"M1","moves":[{"items":[1],"meal_type":"breakfast"},{"items":[1,2],"meal_type":"lunch"}]}`,
		"same meal type twice":    `{"op":"move_items","target":"M1","moves":[{"items":[1],"meal_type":"breakfast"},{"items":[2],"meal_type":"breakfast"}]}`,
		"brunch":                  `{"op":"set_meal_type","target":"M1","meal_type":"brunch"}`,
		"empty moves":             `{"op":"move_items","target":"M1","moves":[]}`,
	}
	for name, plan := range bad {
		t.Run(name, func(t *testing.T) {
			f := newEditFixture(t)
			f.seedF(t, regroupUser)
			f.plan.answers = []string{plan}
			reply := f.sayAs(t, regroupUser, "幫我分成早午晚三餐")
			mustContain(t, reply, editInvalidMessage)
			if f.store.Puts.Load() != 0 {
				t.Error("a card was offered for an invalid plan")
			}
			f.assertNothingWritten(t, 1, 0)
		})
	}
}

// 規格 #10：出卡後別處把來源改了（版本變）→ editStaleMessage，Insert / Update / Delete 呼叫數皆 0，DB 與出卡前相同.
func TestRegroupStaleSource(t *testing.T) {
	f := newEditFixture(t)
	src := f.seedF(t, regroupUser)
	ok := f.offerRegroup(t, regroupUser, src.ID, planOneToBreakfast)
	f.meals.EditStored(0, func(m *meal.Meal) { m.Note = "LIFF 改過" }) // 版本 3 → 4
	fm, reply := f.tapWith(t, ok, nil)
	if reply != editStaleMessage {
		t.Errorf("reply = %q, want %q", reply, editStaleMessage)
	}
	if c := fm.counts(); c != [4]int{} {
		t.Errorf("writes = %v, want none", c)
	}
	if got := dbShape(f); got != "dinner:1810/5" || f.mealByID(src.ID).Version != src.Version+1 {
		t.Errorf("db = %s, want the source untouched by the card", got)
	}
}

// 規格 #11：出卡後併入目標被改 → editStaleMessage、零寫入（來源與新紀錄都沒動）.
func TestRegroupStaleMergeTarget(t *testing.T) {
	f := newEditFixture(t)
	src := f.seedF(t, regroupUser)
	lunch := f.seedSlot(t, regroupUser, "lunch", "既有午餐便當", rgToday(12, 0), 600)
	ok := f.offerRegroup(t, regroupUser, src.ID, planChickenLunch)
	f.meals.EditStored(1, func(m *meal.Meal) { m.Note = "LIFF 改過" })
	fm, reply := f.tapWith(t, ok, nil)
	if reply != editStaleMessage {
		t.Errorf("reply = %q, want %q", reply, editStaleMessage)
	}
	if c := fm.counts(); c != [4]int{} {
		t.Errorf("writes = %v, want none", c)
	}
	if got := dbShape(f); got != "dinner:1810/5,lunch:600/1" {
		t.Errorf("db = %s, want both records untouched", got)
	}
	if f.mealByID(src.ID).Version != src.Version || f.mealByID(lunch.ID).Version != lunch.Version+1 {
		t.Error("versions moved: something was written")
	}
}

// 規格 #12：寫入中途失敗 → 已 Insert 的新紀錄全撤回、來源不變、回失敗句（不含已調整 / 已改 / 今日累計）.
func TestRegroupRollbackRemovesInserted(t *testing.T) {
	f := newEditFixture(t)
	src := f.seedF(t, regroupUser)
	ok := f.offerRegroup(t, regroupUser, src.ID, planThreeDinner)
	fm, reply := f.tapWith(t, ok, func(fm *faultMeals) {
		fm.updateHook = func(n int, _ *meal.Meal) *override { return &override{err: errors.New("mongo timeout")} } // S 來源 Update
	})
	if reply != editFailedMessage {
		t.Errorf("reply = %q, want %q", reply, editFailedMessage)
	}
	assertNoRegroupSuccessClaim(t, reply)
	if got := dbShape(f); got != "dinner:1810/5" {
		t.Errorf("db = %s, want only the untouched source (the two inserted records must be removed)", got)
	}
	if c := fm.counts(); c != [4]int{2, 1, 2, 0} {
		t.Errorf("insert/update/delete/deleteIf = %v, want 2/1/2/0", c)
	}
}

// assertNoRegroupSuccessClaim 失敗類回覆不得含成功用語與今日累計.
func assertNoRegroupSuccessClaim(t *testing.T, reply string) {
	t.Helper()
	assertNoSuccessClaim(t, reply)
	for _, bad := range []string{"已調整", "今日累計"} {
		if strings.Contains(reply, bad) {
			t.Errorf("failure reply contains %q: %s", bad, reply)
		}
	}
}

// 規格 #14：撤回併入目標時它已被別處改過 → 不覆蓋、回 editPartialMessage、log 為 Error 且不含品名與 userID.
func TestRegroupUndoConflictReportsPartial(t *testing.T) {
	const uid = "U0123456789abcdef0123456789abcdef"
	buf := captureSlog(t)
	f := newEditFixture(t)
	src := f.seedF(t, uid)
	lunch := f.seedSlot(t, uid, "lunch", "祕密午餐便當", rgToday(12, 0), 600)
	ok := f.offerRegroup(t, uid, src.ID, planChickenLunch)
	fm := &faultMeals{MealRepository: f.meals}
	fm.updateHook = func(n int, _ *meal.Meal) *override {
		if n != 2 { // 第 1 次是併入目標 L，第 2 次是來源
			return nil
		}
		for i := range f.meals.Items {
			if f.meals.Items[i].ID == lunch.ID {
				f.meals.EditStored(i, func(m *meal.Meal) { m.Note = "別處改過" }) // L 在我們寫入後又被改（v2 → v3）
			}
		}
		return &override{err: errors.New("mongo timeout for " + uid)}
	}
	f.h.mealRepo = fm
	f.tapReply(t, uid, ok)
	reply := replyText(t, f)
	if reply != editPartialMessage {
		t.Errorf("reply = %q, want %q", reply, editPartialMessage)
	}
	assertNoRegroupSuccessClaim(t, reply)
	l := f.mealByID(lunch.ID)
	if l == nil || l.Total.Calories != 900 || l.Note != "別處改過" {
		t.Errorf("lunch = %+v, the undo must not overwrite the other writer's change", l)
	}
	logs := buf.String()
	if !strings.Contains(logs, `"level":"ERROR"`) || !strings.Contains(logs, "undo") {
		t.Errorf("want an Error log about the incomplete undo:\n%s", logs)
	}
	for _, leak := range []string{"祕密午餐便當", "牛肉火腿起司三明治", "雞胸肉", uid, "別處改過"} {
		if strings.Contains(logs, leak) {
			t.Errorf("logs leak %q:\n%s", leak, logs)
		}
	}
}

// 失敗發生在寫入的每一步時的撤回與回覆（規格 §3 表；資料 G = F 另加今天早餐 B 200、午餐 L 600）.
// 計畫甲：[{1→早餐 併入B},{2→午餐 併入L},{3→點心 新開W}]，來源剩第 4、5 項（860）走 Update；
// 計畫乙：[{1→早餐},{2,3→午餐},{4,5→點心}]，來源清空走 DeleteIfVersion.
func TestRegroupFailureAtEachStep(t *testing.T) {
	const (
		planJia = `{"op":"move_items","target":"M1","moves":[{"items":[1],"meal_type":"breakfast"},{"items":[2],"meal_type":"lunch"},{"items":[3],"meal_type":"snack"}]}`
		planYi  = `{"op":"move_items","target":"M1","moves":[{"items":[1],"meal_type":"breakfast"},{"items":[2,3],"meal_type":"lunch"},{"items":[4,5],"meal_type":"snack"}]}`
		before  = "breakfast:200/1,dinner:1810/5,lunch:600/1"
	)
	boom := errors.New("mongo timeout")
	rows := []struct {
		name      string
		plan      string
		setup     func(fm *faultMeals)
		wantReply string
		wantDB    string
		wantCount [4]int
		extra     func(t *testing.T, f *editFixture)
	}{
		{
			"1 P1 source FindByID returns nil", planJia,
			func(fm *faultMeals) {
				fm.findHook = func(n int, _ string) (*meal.Meal, bool) { return nil, n == 1 }
			}, editGoneMessage, before,
			[4]int{},
			nil,
		},
		{
			"2 P1 merge target FindByID returns nil", planJia,
			func(fm *faultMeals) {
				fm.findHook = func(n int, _ string) (*meal.Meal, bool) { return nil, n == 2 }
			}, editGoneMessage, before,
			[4]int{},
			nil,
		},
		{
			"3 M1 Update fails, inserted W is removed", planJia,
			func(fm *faultMeals) {
				fm.updateHook = func(n int, _ *meal.Meal) *override {
					if n == 1 {
						return &override{err: boom}
					}
					return nil
				}
			}, editFailedMessage, before,
			[4]int{1, 1, 1, 0},
			nil,
		},
		{
			"4 M2 Update fails, M1 restored from the snapshot and W removed", planJia,
			func(fm *faultMeals) {
				fm.updateHook = func(n int, _ *meal.Meal) *override {
					if n == 2 {
						return &override{err: boom}
					}
					return nil
				}
			}, editFailedMessage, before,
			[4]int{1, 3, 1, 0},
			func(t *testing.T, f *editFixture) {
				for _, m := range f.mealsOfType("breakfast") {
					if len(m.Items) != 1 || m.Items[0].Name != "既有早餐" || m.Total.Calories != 200 {
						t.Errorf("breakfast B = %+v, want the snapshot content written back", m)
					}
				}
			},
		},
		{
			"5 S Update ok=false, M and W undone", planJia,
			func(fm *faultMeals) {
				fm.updateHook = func(n int, _ *meal.Meal) *override {
					if n == 3 {
						return &override{ok: false}
					}
					return nil
				}
			}, editGoneMessage, before,
			[4]int{1, 5, 1, 0},
			nil,
		},
		{
			"6a S DeleteIfVersion conflict", planYi,
			func(fm *faultMeals) {
				fm.deleteIfHook = func(int) *override { return &override{err: meal.ErrConflict} }
			}, editStaleMessage, before,
			[4]int{1, 4, 1, 1},
			nil,
		},
		{
			"6b S DeleteIfVersion gone", planYi,
			func(fm *faultMeals) {
				fm.deleteIfHook = func(int) *override { return &override{ok: false} }
			}, editGoneMessage, before,
			[4]int{1, 4, 1, 1},
			nil,
		},
		{
			"6c S DeleteIfVersion error", planYi,
			func(fm *faultMeals) {
				fm.deleteIfHook = func(int) *override { return &override{err: boom} }
			}, editFailedMessage, before,
			[4]int{1, 4, 1, 1},
			nil,
		},
		{
			"7 undo of W fails too: partial", planJia,
			func(fm *faultMeals) {
				fm.updateHook = func(n int, _ *meal.Meal) *override {
					if n == 3 {
						return &override{ok: false}
					}
					return nil
				}
				fm.deleteHook = func(int, string) *override { return &override{err: boom} }
			}, editPartialMessage, before + ",snack:300/1",
			[4]int{1, 5, 1, 0},
			nil,
		},
		{
			"8 undo finds the first merge target already gone: skipped, not partial", planJia,
			func(fm *faultMeals) {
				fm.updateHook = func(n int, _ *meal.Meal) *override {
					if n == 3 {
						return &override{ok: false}
					}
					return nil
				}
				// FindByID 呼叫序：P1 來源 #1、B #2、L #3；撤回倒著走：L #4、B #5.
				fm.findHook = func(n int, _ string) (*meal.Meal, bool) { return nil, n == 5 }
			}, editGoneMessage, "breakfast:650/2,dinner:1810/5,lunch:600/1",
			[4]int{1, 4, 1, 0},
			nil,
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f := newEditFixture(t)
			src := f.seedF(t, regroupUser)
			f.seedSlot(t, regroupUser, "breakfast", "既有早餐", rgToday(7, 30), 200)
			f.seedSlot(t, regroupUser, "lunch", "既有午餐", rgToday(12, 0), 600)
			ok := f.offerRegroup(t, regroupUser, src.ID, r.plan)
			fm, reply := f.tapWith(t, ok, r.setup)
			if reply != r.wantReply {
				t.Errorf("reply = %q, want %q", reply, r.wantReply)
			}
			assertNoRegroupSuccessClaim(t, reply)
			if got := dbShape(f); got != r.wantDB {
				t.Errorf("db = %s, want %s", got, r.wantDB)
			}
			if c := fm.counts(); c != r.wantCount { // 預檢失敗的列 wantCount 是零值：連一次寫入類呼叫都不能有
				t.Errorf("insert/update/delete/deleteIf = %v, want %v", c, r.wantCount)
			}
			if r.extra != nil {
				r.extra(t, f)
			}
		})
	}
}

// raceStore PeekPending 讀到之後停住，等測試先讓按鈕把卡 Take 走，再放行（讓「按鈕與打字競爭同一張卡」的順序確定而不靠運氣）.
type raceStore struct {
	recordedit.Store
	peeked  chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (r *raceStore) PeekPending(ctx context.Context, userID string, now time.Time) (*recordedit.Pending, error) {
	p, err := r.Store.PeekPending(ctx, userID, now)
	if err == nil {
		r.once.Do(func() { close(r.peeked) })
		<-r.proceed
	}
	return p, err
}

// 規格 #17：同一張卡按鈕與打字同時確認 → 只寫一次，輸的一方回 editExpiredMessage.
func TestRegroupDoubleConfirmAppliesOnce(t *testing.T) {
	t.Run("deterministic: typed confirm loses the Take to the button", func(t *testing.T) {
		f := newEditFixture(t)
		src := f.seedF(t, regroupUser)
		ok := f.offerRegroup(t, regroupUser, src.ID, planOneToBreakfast)
		rs := &raceStore{Store: f.store, peeked: make(chan struct{}), proceed: make(chan struct{})}
		f.h.editStore = rs
		ll := newLockedLine(t)
		f.h.line = ll.client
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.h.handleTextMessage(context.Background(), regroupUser, "確認", "tok-typed")
		}()
		<-rs.peeked // 打字那邊已 Peek 到卡、卡還在
		f.h.handleEventSafely(context.Background(), postbackEvent(regroupUser, ok, time.Now()))
		close(rs.proceed) // 按鈕先把卡取走並寫完，才放打字去 Take
		<-done

		var applied, expired int
		for _, r := range ll.all() {
			switch {
			case strings.Contains(r, "已調整"):
				applied++
			case strings.Contains(r, editExpiredMessage):
				expired++
			}
		}
		if applied != 1 || expired != 1 {
			t.Errorf("applied=%d expired=%d, want 1/1: %v", applied, expired, ll.all())
		}
		if got := dbShape(f); got != "breakfast:450/1,dinner:1360/4" {
			t.Errorf("db = %s, the split must be written exactly once", got)
		}
	})
	t.Run("race: button and typed at the same time, repeated", func(t *testing.T) {
		for round := range 25 {
			f := newEditFixture(t)
			src := f.seedF(t, regroupUser)
			ok := f.offerRegroup(t, regroupUser, src.ID, planOneToBreakfast)
			ll := newLockedLine(t)
			f.h.line = ll.client
			var wg sync.WaitGroup
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				f.h.handleEventSafely(context.Background(), postbackEvent(regroupUser, ok, time.Now()))
			}()
			go func() {
				defer wg.Done()
				<-start
				f.h.handleTextMessage(context.Background(), regroupUser, "確認", "tok-typed")
			}()
			close(start)
			wg.Wait()
			applied := 0
			for _, r := range ll.all() {
				if strings.Contains(r, "已調整") {
					applied++
				}
			}
			if applied != 1 || len(ll.all()) != 2 {
				t.Fatalf("round %d: applied=%d replies=%v, want exactly one success and one loser reply", round, applied, ll.all())
			}
			for _, r := range ll.all() {
				if !strings.Contains(r, "已調整") && !strings.Contains(r, editExpiredMessage) && !strings.Contains(r, editNoPendingMessage) {
					t.Fatalf("round %d: unexpected loser reply %s", round, r)
				}
			}
			if got := dbShape(f); got != "breakfast:450/1,dinner:1360/4" {
				t.Fatalf("round %d: db = %s, written more than once or not at all", round, got)
			}
		}
	})
}

// 規格 #24：寫入進行中（W1 成功後）刪資料標記出現 → 已 Insert 的 ID 被補刪、回 erasedMessage、不出現成功句；補刪失敗改回 eraseFailedMessage.
func TestRegroupErasureMidWriteUndoes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		deleteErr error
		want      string
		wantDB    string
	}{
		{"undo succeeds", nil, erasedMessage, "dinner:860/2"},
		{"undo fails", errors.New("mongo timeout"), eraseFailedMessage, "breakfast:450/1,dinner:860/2,lunch:500/2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEditFixture(t)
			src := f.seedF(t, regroupUser)
			ok := f.offerRegroup(t, regroupUser, src.ID, planThreeDinner)
			fm := &faultMeals{MealRepository: f.meals}
			fm.afterInsert = func(n int) {
				if n == 1 {
					markErased(t, f.h.erased, context.Background(), regroupUser) // W1 成功之後客人刪了資料
				}
			}
			if tc.deleteErr != nil {
				fm.deleteHook = func(int, string) *override { return &override{err: tc.deleteErr} }
			}
			f.h.mealRepo = fm
			// 事件時間設在一分鐘前：標記（現在）比這則訊息的送出時間新.
			f.h.handleEventSafely(context.Background(), postbackEvent(regroupUser, ok, time.Now().Add(-time.Minute)))
			reply := replyText(t, f)
			if reply != tc.want {
				t.Errorf("reply = %q, want %q", reply, tc.want)
			}
			assertNoRegroupSuccessClaim(t, reply)
			if got := dbShape(f); got != tc.wantDB {
				t.Errorf("db = %s, want %s (the records inserted after the erasure must be removed unless the cleanup failed)", got, tc.wantDB)
			}
		})
	}
}

// 候選選定後也走同一份分組邏輯：兩筆今天的餐點讓規劃器出候選 → 點 D → 出 regroup 確認卡 → 確認寫入.
// 走真的規劃器與真的時鐘，所以只斷言筆數、餐別、合計，不斷言精確時刻（dispatch「時鐘」條）.
func TestRegroupPickCandidateBuildsRegroup(t *testing.T) {
	f := newEditFixture(t)
	d := f.seedF(t, regroupUser)
	f.seedSlot(t, regroupUser, "lunch", "既有午餐便當", rgToday(12, 0), 600)
	f.plan.answers = []string{`{"op":"move_items","candidates":["M1","M2"],"moves":[{"items":[1],"meal_type":"breakfast"}]}`}
	f.sayAs(t, regroupUser, "把三明治移到早餐")
	var pickD string
	for _, a := range f.quickReplies(t) {
		if data, _ := a["data"].(string); strings.HasPrefix(data, "ce1:pick:") && strings.HasSuffix(data, ":0") {
			pickD = data // 候選依時間由新到舊：M1 = 晚餐 D
		}
	}
	if pickD == "" {
		t.Fatalf("no pick button in %s", f.api.lastReply(t))
	}
	f.tapReply(t, regroupUser, pickD)
	mustContain(t, replyText(t, f), "要這樣調整 "+rgDate()+" 的餐點嗎？", "・早餐：「牛肉火腿起司三明治」→ 新增 450 大卡", "・晚餐：剩 1360 大卡（原 1810）")
	ok := f.buttonData(t, "ok")
	if ok == "" {
		t.Fatalf("no confirm button after picking: %s", f.api.lastReply(t))
	}
	if f.meals.Updates != 0 || len(f.meals.Items) != 2 {
		t.Fatal("picking a candidate must not write")
	}
	f.tapReply(t, regroupUser, ok)
	if got := dbShape(f); got != "breakfast:450/1,dinner:1360/4,lunch:600/1" {
		t.Fatalf("db = %s", got)
	}
	if m := f.mealByID(d.ID); m == nil || m.Total.Calories != 1360 {
		t.Errorf("source = %+v", m)
	}
	if b := f.mealsOfType("breakfast"); len(b) != 1 || b[0].EatenAt.In(nutrition.Taipei).Format("01/02") != rgDate() {
		t.Errorf("new breakfast = %+v, want it on today's date", b)
	}
	if !strings.HasSuffix(replyText(t, f), "\n今日累計：2410 大卡（3 餐）") {
		t.Errorf("reply = %q", replyText(t, f))
	}
}
