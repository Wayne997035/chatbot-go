package webhook

import (
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"context"
	"testing"
)

// 以下測試模擬「讀出之後、寫回之前，另一個請求（LIFF 或另一則訊息）先寫入了」.
// 以前是讀出整筆、改完整筆寫回，後寫的會把先寫的修改蓋掉.

var userPlace = &meal.Place{Name: "老張牛肉麵", Source: meal.PlaceFromUser}

// 「再加一顆滷蛋」估算的幾秒間，使用者在別處記了店家：兩個修改都要留下.
func TestAddingFoodKeepsAPlaceSetMeanwhile(t *testing.T) {
	f := newMealFixture(t, noodleText, eggText)
	f.say(t, "午餐吃了牛肉麵")
	f.meals.beforeUpdate = func(r *fakeMealRepo) {
		r.beforeUpdate = nil
		r.editStored(func(m *meal.Meal) { m.Place = userPlace })
	}

	reply := f.say(t, "再加一顆滷蛋")

	got := f.meals.meals[0]
	if len(got.Items) != 2 || got.Total.Calories != 725 {
		t.Errorf("the egg must be added: %+v", got.Items)
	}
	if got.Place == nil || got.Place.Name != "老張牛肉麵" {
		t.Errorf("the place set meanwhile must be kept: %+v", got.Place)
	}
	if f.meals.updates != 2 {
		t.Errorf("updates = %d, want a conflict then a retry", f.meals.updates)
	}
	mustContain(t, reply, "已加到剛剛的午餐", "合計：725 大卡")
}

// 改份量時，剛在別處加的品項也一起按比例調整，不會不見（熱量四捨五入到整數：37.5 → 38）.
func TestPortionChangeKeepsFoodAddedMeanwhile(t *testing.T) {
	f := newMealFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")
	f.meals.beforeUpdate = func(r *fakeMealRepo) {
		r.beforeUpdate = nil
		r.editStored(func(m *meal.Meal) {
			m.Items = append(m.Items, meal.FoodItem{Name: "滷蛋", Portion: "1顆", Nutrients: meal.Nutrients{Calories: 75}})
			m.Rebase()
		})
	}

	reply := f.say(t, keywordHalf)

	got := f.meals.meals[0]
	if len(got.Items) != 2 || got.Total.Calories != 325+38 || got.Scale != 0.5 {
		t.Errorf("both items are halved: total=%v scale=%v items=%+v", got.Total.Calories, got.Scale, got.Items)
	}
	mustContain(t, reply, "已改成一半的份量")
}

// 補拍照片分析的期間使用者自己輸入了店名：照片辨識的店家不能蓋掉使用者給的.
func TestPhotoMergeKeepsAPlaceTypedMeanwhile(t *testing.T) {
	f := newMealFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")
	f.meals.beforeUpdate = func(r *fakeMealRepo) {
		r.beforeUpdate = nil
		r.editStored(func(m *meal.Meal) { m.Place = userPlace })
	}

	f.photo(t, noodlePhoto) // 照片辨識出「林東芳」

	got := f.meals.meals[0]
	if got.Place == nil || got.Place.Name != "老張牛肉麵" || got.Place.Source != meal.PlaceFromUser {
		t.Errorf("the user's place wins: %+v", got.Place)
	}
	if got.Items[0].Name != "清燉牛肉麵" || got.Source != meal.SourcePhotoText {
		t.Errorf("the photo estimate is applied: %+v", got)
	}
}

// 一直衝突時最多重試 3 次就停，請使用者再試一次（不會無限重試，也不會蓋掉別人的資料）.
func TestMealEditGivesUpAfterRepeatedConflicts(t *testing.T) {
	f := newMealFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")
	f.meals.beforeUpdate = func(r *fakeMealRepo) {
		r.editStored(func(m *meal.Meal) { m.Note = "別處的修改" })
	}

	reply := f.say(t, keywordHalf)

	mustContain(t, reply, "請再試一次")
	if f.meals.updates != 3 {
		t.Errorf("updates = %d, want 3", f.meals.updates)
	}
	if got := f.meals.meals[0]; got.Scale != 0 || got.Total.Calories != 650 || got.Note != "別處的修改" {
		t.Errorf("nothing of ours is written: %+v", got)
	}
}

// 修改期間這筆被刪除：說找不到，不會把它寫回來.
func TestEditingAMealDeletedMeanwhile(t *testing.T) {
	f := newMealFixture(t, noodleText)
	f.say(t, "午餐吃了牛肉麵")
	f.meals.beforeUpdate = func(r *fakeMealRepo) { r.meals = nil }

	reply := f.say(t, keywordHalf)

	mustContain(t, reply, "找不到 30 分鐘內記錄的餐點")
	if len(f.meals.meals) != 0 {
		t.Errorf("a deleted meal must not come back: %+v", f.meals.meals)
	}
}

// 份量按鈕的每個倍數都必須是 SetScale 接受的值，否則會回覆「已改成」但其實沒改.
func TestEveryPortionWordIsAValidScale(t *testing.T) {
	for word, scale := range portionScales {
		if !meal.ValidScale(scale) {
			t.Errorf("%q → %v is outside %v～%v", word, scale, meal.MinScale, meal.MaxScale)
		}
	}
}

// 連續傳「身高 175」「體重 70」兩則訊息同時處理：兩個欄位都要留下.
func TestProfileFieldsSentTogetherAreBothKept(t *testing.T) {
	f := newFitnessFixture(t, "")
	f.profiles.profiles["U1"] = &profile.Profile{UserID: "U1", Age: 30}
	f.profiles.beforeWrite = func() {
		f.h.updateProfile(context.Background(), "U1", "tok", &fitness.ProfileUpdate{HeightCM: 175})
	}

	reply := f.say(t, "體重 70")

	got := f.profiles.profiles["U1"]
	if got.WeightKG != 70 || got.HeightCM != 175 || got.Age != 30 {
		t.Errorf("every field must survive: %+v", got)
	}
	mustContain(t, reply, "70", "175")
}
