//go:build liveeval

package assistant

import (
	"chatbot-go/internal/assistant/recordedit"
	"chatbot-go/internal/storage/database/meal"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// [F33-41] user：「一開始預估很低 然後看ai分析後能不能改正 這才是重點」「這些都得使用者主動跟bot說要更新」
// 「ai分析還是照原本的沒問題 只是後續功能要能協助改正計算正確」.
// 每個類別：原本的紀錄（看圖或打字記的，估得偏低或偏高）＋客人主動要求更新的一句 → 改完的總熱量要落在合理範圍、改對那一筆.
// 預期值依據（每 100 公克，USDA 資料轉載）：奶油乳酪約 350–367、鮮奶油（重乳脂）約 336–343、花生醬約 589、
// 凱薩醬約 325、白吐司約 238 大卡；標「未查官方數值」的類別只驗方向與保守的幅度.

// correctionSpec 一個改正類別.
type correctionSpec struct {
	name, say   string
	target      meal.Meal   // 要被改的那筆
	others      []meal.Meal // 同時存在的其他紀錄（測改對那一筆）
	lo, hi      float64     // 改完總熱量的合理範圍
	perProvider bool        // 每家供應商各跑一次
}

func correctionMeal(id, name string, kcal float64, at time.Time) meal.Meal {
	return meal.Meal{
		ID: id, UserID: "U1", Version: 1, MealType: "lunch", EatenAt: at,
		Items: []meal.FoodItem{{Name: name, Portion: "1份", Nutrients: meal.Nutrients{Calories: kcal}}}, Total: meal.Nutrients{Calories: kcal},
	}
}

func correctionSpecs(now time.Time) []correctionSpec {
	bun := correctionMeal("bun", "餐包", 150, now.Add(-time.Hour))
	toast := correctionMeal("toast", "吐司", 80, now.Add(-3*time.Hour))
	noodle := correctionMeal("noodle", "牛肉麵", 550, now.Add(-time.Hour))
	return []correctionSpec{
		// 奶油乳酪、鮮奶油各 30 公克就多約 200 大卡；包餡至少 +100.
		{name: "C1 餐包包起司鮮奶油餡", say: "餐包裡面其實有包起司鮮奶油餡，幫我更新熱量", target: bun, others: []meal.Meal{toast}, lo: 250, hi: 1000, perProvider: true},
		// 花生醬 20 公克約 118 大卡；吐司比餐包舊，要改到 M2 不是最新那筆.
		{name: "C2 吐司抹厚花生醬", say: "吐司其實有抹很厚的花生醬，幫我更新", target: toast, others: []meal.Meal{bun}, lo: 160, hi: 600},
		// 凱薩醬 30 公克約 100 大卡.
		{name: "C3 沙拉淋凱薩醬", say: "沙拉淋了很多凱薩醬，幫我改一下熱量", target: correctionMeal("salad", "生菜沙拉", 60, now.Add(-time.Hour)), lo: 140, hi: 600},
		// 小菜（滷海帶、豆干）：未查官方數值，保守下限 +50.
		{name: "C4 牛肉麵加點小菜", say: "我還有點一盤小菜，滷海帶跟豆干，幫我加上去", target: noodle, lo: 600, hi: 1200},
		// 酸菜量少：未查官方數值，只驗往上且不離譜（最多 +150）.
		{name: "C5 牛肉麵加酸菜", say: "牛肉麵有加酸菜，幫我算進去", target: noodle, lo: 555, hi: 700},
		// 湯喝完／沒喝：未查官方數值，只驗方向與至少 30 大卡.
		{name: "C6 牛肉麵湯喝完", say: "牛肉麵的湯我全部喝完了，幫我補上", target: noodle, lo: 580, hi: 1000},
		{name: "C7 牛肉麵湯沒喝", say: "牛肉麵的湯我都沒喝，幫我扣掉", target: noodle, lo: 250, hi: 520},
		// 份量倍數：兩個 = 300±10%，一半 = 400±10%.
		{name: "C8 餐包吃了兩個", say: "餐包我其實吃了兩個，幫我調整", target: bun, lo: 270, hi: 330},
		{name: "C9 便當只吃一半", say: "便當我只吃了一半，熱量幫我減半", target: correctionMeal("bento", "便當", 800, now.Add(-time.Hour)), lo: 360, hi: 440},
		// 做法：烤雞胸改成炸的，未查官方數值，保守下限 +60.
		{name: "C10 雞胸肉其實是炸的", say: "雞胸肉其實是炸的，重新算一下", target: correctionMeal("chicken", "烤雞胸肉", 165, now.Add(-time.Hour)), lo: 225, hi: 700},
		// 飲料：無糖改全糖加珍珠，未查官方數值，保守下限 +100.
		{name: "C11 紅茶全糖加珍珠", say: "紅茶其實是全糖還加了珍珠，幫我重算", target: correctionMeal("tea", "無糖紅茶", 10, now.Add(-time.Hour)), lo: 110, hi: 800},
	}
}

// correctionRed 判紅：要出確認卡、改對那一筆、總熱量在範圍內；新增品項時總熱量必須剛好等於原本 + 品項.
func correctionRed(s *correctionSpec, o *recordedit.Outcome) string {
	base := s.target.Total.Calories
	switch {
	case !isProposal(o):
		return "不是 Proposal（客人主動要求更新卻沒出確認卡）"
	case o.Proposal.RecordID != s.target.ID:
		return fmt.Sprintf("改到別筆 %s，應該是 %s", o.Proposal.RecordID, s.target.ID)
	case o.Proposal.After < s.lo || o.Proposal.After > s.hi:
		return fmt.Sprintf("改完 %.0f 大卡，不在 %.0f～%.0f（原本 %.0f）", o.Proposal.After, s.lo, s.hi, base)
	case o.Proposal.Op == recordedit.OpAddItem && (o.Proposal.Plan.NewItem == nil || o.Proposal.After != base+math.Round(o.Proposal.Plan.NewItem.Calories)):
		return "新增品項的總熱量不等於原本 + 品項熱量"
	}
	return ""
}

func correctionCases(now time.Time) []editCase {
	specs := correctionSpecs(now)
	out := make([]editCase, 0, len(specs))
	for i := range specs {
		s := &specs[i]
		out = append(out, editCase{
			name: s.name, say: s.say, meals: append([]meal.Meal{s.target}, s.others...), perProvider: s.perProvider,
			red: func(o *recordedit.Outcome) string { return correctionRed(s, o) },
		})
	}
	return out
}

func addItemOutcome(id string, after, item float64) *recordedit.Outcome {
	o := propOutcome(id, recordedit.OpAddItem, after)
	o.Proposal.Plan.NewItem = &recordedit.NewItem{Name: "加料", Calories: item}
	return o
}

// correctionSamples 每個類別的正反例（給 TestRecordEditReplayHoldCheck 證明判紅會叫）.
func correctionSamples(now time.Time) map[string]struct{ green, red []*recordedit.Outcome } {
	out := map[string]struct{ green, red []*recordedit.Outcome }{}
	specs := correctionSpecs(now)
	for i := range specs {
		s := &specs[i]
		id, base, mid := s.target.ID, s.target.Total.Calories, math.Round((s.lo+s.hi)/2)
		green := []*recordedit.Outcome{propOutcome(id, recordedit.OpSetItemKcal, mid)}
		red := []*recordedit.Outcome{
			{Kind: recordedit.OutcomeNone},
			{Kind: recordedit.OutcomeInvalid},
			propOutcome(id, recordedit.OpSetItemKcal, s.lo-1), propOutcome(id, recordedit.OpSetItemKcal, s.hi+1),
			propOutcome("other", recordedit.OpSetItemKcal, mid), propOutcome(id, recordedit.OpSetItemKcal, base),
		}
		if mid > base {
			green = append(green, addItemOutcome(id, mid, mid-base))
			red = append(red, addItemOutcome(id, mid, mid-base+1))
		}
		out[s.name] = struct{ green, red []*recordedit.Outcome }{green, red}
	}
	return out
}

// allEstimates 回覆裡所有熱量數字（去掉每 100 公克參考值與舉例）；不套 sliceEstimates 的已知值略過表（含 500）.
func allEstimates(reply string) []int {
	s := exampleRe.ReplaceAllString(per100Re.ReplaceAllString(perKcalRe.ReplaceAllString(plainSpaces(reply), ""), ""), "")
	var out []int
	for _, m := range kcalRe.FindAllStringSubmatch(s, -1) {
		for _, g := range m[1:] {
			var n int
			if _, err := fmt.Sscanf(g, "%d", &n); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// qaCorrectionRed 問答（規劃器沒接住時的退路）：往上修的類別最大估算要到 lo、不超過 hi；往下修的類別要有一個估算落在範圍內.
func qaCorrectionRed(s *correctionSpec, reply string) string {
	// [F33-46] 決策 2805c816：往上、往下都改成「有一個估算落在範圍內」；取最大值會抓到今日淨值赤字這類非估算數字（C8 假紅）.
	nums := allEstimates(reply)
	for _, n := range nums {
		if float64(n) >= s.lo && float64(n) <= s.hi {
			return ""
		}
	}
	return fmt.Sprintf("沒有任何估算落在 %.0f～%.0f（%v）", s.lo, s.hi, nums)
}

// TestCorrectionQACheck 證明問答判紅與「宣稱已改」檢查會叫（不呼叫模型、不需要金鑰）.
func TestCorrectionQACheck(t *testing.T) {
	specs := correctionSpecs(time.Now())
	up, two, down := &specs[0], &specs[7], &specs[8] // C1 餐包包餡（往上）、C8 吃兩個、C9 便當一半（往下）
	for _, c := range []struct {
		s     *correctionSpec
		reply string
		red   bool
	}{
		{up, "餐包約 150 大卡。", true},
		{up, "依你說的包了起司鮮奶油餡，餐包約 150 大卡＋餡料約 200 大卡，合計約 350 大卡。", false},
		{up, "整個約 500 大卡。", false},
		{up, "約 1500 大卡。", true},
		{two, "兩個約 300 大卡，今天熱量赤字約 1284 大卡。", false},
		{two, "一個約 150 大卡，今天熱量赤字約 1284 大卡。", true},
		{down, "原本 800 大卡，吃一半約 400 大卡。", false},
		{down, "便當約 800 大卡。", true},
	} {
		if got := qaCorrectionRed(c.s, c.reply) != ""; got != c.red {
			t.Errorf("%s %q：判紅=%v，要 %v", c.s.name, c.reply, got, c.red)
		}
	}
	for _, s := range []string{"好的，已更新為 400 大卡。", "已改成 400 大卡", "我已為您更新紀錄"} {
		if falseUpdateClaim(s) == "" {
			t.Errorf("宣稱已改卻沒抓到：%q", s)
		}
	}
	for _, s := range []string{"你今天已記錄 2 餐。", "要修改紀錄，直接告訴我要改哪一筆、改成多少，我會請你確認。", "依新資訊把熱量修正為約 250 大卡。"} {
		if c := falseUpdateClaim(s); c != "" {
			t.Errorf("沒有宣稱已改卻被抓（%s）：%q", c, s)
		}
	}
}

// TestLiveCorrectionQA 規劃器沒接住時，問答也要把估算往正確方向改，而且不准宣稱已改（liveAsk 內檢查）.
func TestLiveCorrectionQA(t *testing.T) {
	specs := correctionSpecs(time.Now())
	for _, i := range []int{0, 7, 8} { // C1 包餡、C8 兩個、C9 一半
		s := &specs[i]
		a, _ := liveQA(t, append([]meal.Meal{s.target}, s.others...))
		reply := liveAsk(t, a, strings.Replace(s.say, "幫我", "請", 1))
		if r := qaCorrectionRed(s, reply); r != "" {
			t.Errorf("[%s] %s", s.name, r)
		}
	}
}

// TestLiveTodayTotalAfterEdit [F33-41] user：「更新後得馬上查證 本日消耗 看有沒有更正」「也就是快取 嗎」.
// 餐點與今日總數不快取（每次查 MongoDB），但對話紀錄會留 30 分鐘：紀錄已改成 400、對話裡還有先前說的 150／650，
// 問今天吃了多少 → 要照「使用者資料」回新的 900，不准沿用對話裡的舊總數.
func TestLiveTodayTotalAfterEdit(t *testing.T) {
	now := time.Now()
	meals := []meal.Meal{
		correctionMeal("bento", "便當", 500, now.Add(-2*time.Minute)),
		correctionMeal("bun", "餐包", 150, now.Add(-time.Minute)),
	}
	meals[1].Items = append(meals[1].Items, meal.FoodItem{Name: "起司鮮奶油餡", Portion: "1份", Nutrients: meal.Nutrients{Calories: 250}})
	meals[1].Total.Calories = 400
	a, h := liveQA(t, meals)
	h.turns["U1"] = []Turn{{Question: "今天吃了多少", Answer: "你今天記錄了便當 500 大卡、餐包 150 大卡，共 650 大卡。"}}
	reply := liveAsk(t, a, "今天總共吃了多少熱量")
	if r := todayTotalRed(reply); r != "" {
		t.Error(r)
	}
}

// todayTotalRed 回覆一定要有改後的今日總數 900（只有舊的 650 = 沿用對話裡的舊數字）.
func todayTotalRed(reply string) string {
	for _, n := range allEstimates(reply) {
		if n == 900 {
			return ""
		}
	}
	return fmt.Sprintf("今日總數沒有回改後的 900（%v）", allEstimates(reply))
}

// TestTodayTotalCheck 證明 todayTotalRed 會叫（不呼叫模型）.
func TestTodayTotalCheck(t *testing.T) {
	for _, s := range []string{"你今天共攝取 650 大卡。", "今天大約 700 大卡。"} {
		if todayTotalRed(s) == "" {
			t.Errorf("該紅卻沒紅：%q", s)
		}
	}
	for _, s := range []string{"你今天共攝取 900 大卡（2 餐）。", "原本 650 大卡，餐包改成 400 後，今天共 900\u202f大卡。", "之前說的 650 大卡已經不是現在的數字，今天共 900 大卡。"} {
		if r := todayTotalRed(s); r != "" {
			t.Errorf("正確回覆被判紅：%s", r)
		}
	}
}
