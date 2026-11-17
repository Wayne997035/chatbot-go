package assistant

import (
	"chatbot-go/internal/storage/database/meal"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func pastMeal(uid, name string, kcal float64, ago time.Duration, items ...string) meal.Meal {
	m := meal.Meal{UserID: uid, EatenAt: fixedNow.Add(-ago), Total: meal.Nutrients{Calories: kcal}}
	if name != "" {
		items = append([]string{name}, items...)
	}
	for _, it := range items {
		m.Items = append(m.Items, meal.FoodItem{Name: it})
	}
	return m
}

// [F33-45] 決策 2805c816 ②：問答看得到前幾天每一餐的日期、品名與已記錄熱量（新到舊、最多 12 餐、不含今天、不含別人）.
func TestAnswerListsPastMealsNewestFirst(t *testing.T) {
	// fixedNow = 台北 11/02 20:00.
	meals := []meal.Meal{
		pastMeal("U1", "雞腿便當", 820, time.Hour),         // 今天：只在「今天」那行
		pastMeal("U1", "吐司", 300, 27*time.Hour, "花生醬"), // 11/01 17:00
		pastMeal("U1", "牛肉麵", 650, 24*time.Hour),       // 11/01 20:00，比吐司新
		pastMeal("U1", "", 1800, 48*time.Hour),         // 10/31 沒有品名
		pastMeal("U2", "別人的宵夜", 999, 25*time.Hour),     // 別人的
		pastMeal("U1", "珍珠奶茶", 0, 50*time.Hour),        // 10/31 沒有熱量：只給品名
	}
	for i := 0; i < 12; i++ {
		meals = append(meals, pastMeal("U1", fmt.Sprintf("便當%02d", i), 500, time.Duration(72+i)*time.Hour))
	}
	c := &fakeCompleter{answers: []string{"好"}}
	a, _ := newTestAssistant(c, false)
	a.meals = &stubMeals{meals: meals}
	if _, err := a.Answer(context.Background(), "U1", "昨天那碗牛肉麵湯有喝完"); err != nil {
		t.Fatal(err)
	}
	prompt := c.reqs[0].Prompt
	const head = "前幾天的紀錄（新到舊）："
	at := strings.Index(prompt, head)
	if at < 0 {
		t.Fatalf("prompt has no past meals line:\n%s", prompt)
	}
	line := prompt[at+len(head):]
	line = line[:strings.IndexByte(line+"\n", '\n')]
	got := strings.Split(line, "、")
	want := []string{"11/01 牛肉麵 650 大卡", "11/01 吐司＋花生醬 300 大卡", "10/31 （沒有品名） 1800 大卡", "10/31 珍珠奶茶"}
	for i := 0; i < 8; i++ {
		want = append(want, fmt.Sprintf("10/30 便當%02d 500 大卡", i))
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("past meals =\n%v\nwant\n%v", got, want)
	}
	for _, banned := range []string{"雞腿便當", "別人的宵夜", "便當08"} {
		if strings.Contains(line, banned) {
			t.Errorf("past meals line must not contain %q: %s", banned, line)
		}
	}
	if !strings.Contains(prompt, "今天：攝取 820 大卡（1 餐：雞腿便當") {
		t.Errorf("today line changed:\n%s", prompt)
	}
}

// [F33-45] 只有今天的紀錄時不出現「前幾天」那行.
func TestPastFoodsEmptyWithoutEarlierMeals(t *testing.T) {
	todayOnly := []meal.Meal{pastMeal("U1", "雞腿便當", 820, time.Hour)}
	if got := pastFoods(todayOnly, fixedNow.Add(-2*time.Hour)); got != "" {
		t.Errorf("pastFoods = %q, want empty", got)
	}
	if got := pastFoods(nil, fixedNow); got != "" {
		t.Errorf("pastFoods(nil) = %q, want empty", got)
	}
}
