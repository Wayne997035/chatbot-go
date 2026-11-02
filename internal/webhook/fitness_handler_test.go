package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/richmenu"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/database/workout"
	"chatbot-go/internal/storage/storagetest"
	"chatbot-go/internal/weather"
	"context"
	"strings"
	"testing"
	"time"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

// emptyWeatherRepo 查不到任何地區，讓文字一路落到 AI 運動解析.
type emptyWeatherRepo struct{ fakeWeatherRepo }

func (*emptyWeatherRepo) FindByDistrict(context.Context, string) ([]weatherstore.WeatherForecast, error) {
	return nil, nil
}

type fitnessFixture struct {
	h        *WebhookHandler
	api      *lineAPI
	workouts *storagetest.Workouts
	profiles *storagetest.Profiles
	meals    *storagetest.Meals
}

func newFitnessFixture(t *testing.T, aiAnswer string) *fitnessFixture {
	t.Helper()
	f := &fitnessFixture{
		api:      newLineAPI(t, jpegHeader),
		workouts: &storagetest.Workouts{},
		profiles: storagetest.NewProfiles(),
		meals:    &storagetest.Meals{},
	}
	var parser *fitness.AIParser
	if aiAnswer != "" {
		parser = fitness.NewAIParser(&visionCompleter{answer: aiAnswer})
	}
	f.h = NewWebhookHandler(f.api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil, conversation.NewMemoryManager()).
		WithNutrition(nil, f.meals, nil).
		WithFitness(f.workouts, f.profiles, parser)
	return f
}

// say 模擬使用者送出一則文字（走完整的 handleTextMessage 流程）.
func (f *fitnessFixture) say(t *testing.T, text string) string {
	t.Helper()
	f.h.handleTextMessage(context.Background(), "U1", text, "tok")
	return f.api.lastReply(t)
}

func TestTypedWorkoutIsRecordedWithProfileWeight(t *testing.T) {
	f := newFitnessFixture(t, "")
	f.profiles.Items["U1"] = profile.Profile{UserID: "U1", WeightKG: 70}

	reply := f.say(t, "跑5k 30分")

	if len(f.workouts.Items) != 1 || f.workouts.Items[0].Calories != 343 || f.workouts.Items[0].Source != workout.SourceText {
		t.Fatalf("workouts = %+v", f.workouts.Items)
	}
	for _, want := range []string{"已記錄：跑步 30 分鐘・5 公里", "消耗約 343 大卡（以體重 70 公斤計算）", "今日運動共消耗 343 大卡", "刪除這筆"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q: %s", want, reply)
		}
	}
}

func TestActivityButtonAsksDurationThenRecords(t *testing.T) {
	f := newFitnessFixture(t, "")
	if reply := f.say(t, "記運動"); !strings.Contains(reply, `"text":"重訓"`) {
		t.Fatalf("activity buttons missing: %s", reply)
	}
	if reply := f.say(t, "重訓"); !strings.Contains(reply, "重訓多久呢") || !strings.Contains(reply, "45分鐘") {
		t.Fatalf("duration question: %s", reply)
	}
	if reply := f.say(t, "隨便"); !strings.Contains(reply, "請輸入時間或距離") {
		t.Fatalf("invalid duration should re-ask: %s", reply)
	}
	reply := f.say(t, "45分鐘")
	if len(f.workouts.Items) != 1 || f.workouts.Items[0].Activity != "重訓" || f.workouts.Items[0].DurationMin != 45 {
		t.Fatalf("workouts = %+v", f.workouts.Items)
	}
	if !strings.Contains(reply, "以 60 公斤估算") {
		t.Errorf("should hint default weight: %s", reply)
	}
}

func TestAIParsedWorkoutNeedsConfirmation(t *testing.T) {
	f := newFitnessFixture(t, `{"is_workout":true,"activity":"划船","duration_min":20,"distance_km":0,"calories":0}`)
	reply := f.say(t, "下班練了二十分鐘的划槳")
	if !strings.Contains(reply, "要記錄這筆運動嗎") || !strings.Contains(reply, "划船 20 分鐘") || len(f.workouts.Items) != 0 {
		t.Fatalf("should ask to confirm first: %s", reply)
	}
	f.say(t, keywordConfirm)
	if len(f.workouts.Items) != 1 || f.workouts.Items[0].Source != workout.SourceAI {
		t.Fatalf("workouts = %+v", f.workouts.Items)
	}

	f.say(t, "下班練了二十分鐘的划槳")
	if reply := f.say(t, keywordCancel); !strings.Contains(reply, "已取消") || len(f.workouts.Items) != 1 {
		t.Errorf("cancel: %s", reply)
	}
}

func TestProfileUpdateAndTodayStats(t *testing.T) {
	f := newFitnessFixture(t, "")
	if reply := f.say(t, "體重 70 身高 175 30歲 男"); !strings.Contains(reply, "已更新") || !strings.Contains(reply, "1979 大卡") {
		t.Fatalf("profile: %s", reply)
	}
	f.meals.Items = []meal.Meal{{UserID: "U1", EatenAt: time.Now(), Total: meal.Nutrients{Calories: 900}}}
	f.say(t, "重訓 60分鐘")
	reply := f.say(t, "今日統計")
	for _, want := range []string{"攝取：900 大卡（1 餐）", "運動消耗：350 大卡（重訓 60 分鐘）", "基礎代謝＋日常：1979 大卡", "熱量赤字"} {
		if !strings.Contains(reply, want) {
			t.Errorf("today missing %q: %s", want, reply)
		}
	}
	if reply := f.say(t, "本週統計"); !strings.Contains(reply, "近 7 天統計") || !strings.Contains(reply, "運動消耗 350 大卡（1 次）") {
		t.Errorf("week: %s", reply)
	}
}

func TestDeleteMyDataRequiresConfirmation(t *testing.T) {
	f := newFitnessFixture(t, "")
	f.say(t, "體重 70")
	f.say(t, "跑步 30分鐘")
	if reply := f.say(t, keywordDeleteData); !strings.Contains(reply, "無法復原") {
		t.Fatalf("confirm prompt: %s", reply)
	}
	if reply := f.say(t, "算了"); !strings.Contains(reply, "資料都還在") || len(f.workouts.Items) != 1 {
		t.Fatalf("cancel keeps data: %s", reply)
	}
	f.say(t, keywordDeleteData)
	if reply := f.say(t, "確認刪除"); !strings.Contains(reply, "已刪除") {
		t.Fatalf("delete: %s", reply)
	}
	if len(f.workouts.Items) != 0 || len(f.profiles.Items) != 0 {
		t.Errorf("data not deleted: %+v %+v", f.workouts.Items, f.profiles.Items)
	}
}

func TestWorkoutScreenshotIsRecordedWithAppCalories(t *testing.T) {
	f := newFitnessFixture(t, "")
	f.h.WithNutrition(nil, f.meals, nil)
	h, _ := newFoodHandler(f.api.client, `{"kind":"exercise","foods":[],"exercise":{"activity":"Outdoor Run","duration_min":32,"distance_km":5.2,"calories":356}}`, f.meals)
	h.WithFitness(f.workouts, f.profiles, nil)

	h.handleMessage(context.Background(), imageEvent())

	reply := f.api.lastReply(t)
	if len(f.workouts.Items) != 1 || f.workouts.Items[0].Calories != 356 || f.workouts.Items[0].Activity != "跑步" {
		t.Fatalf("workouts = %+v", f.workouts.Items)
	}
	if !strings.Contains(reply, "依運動 App 數據") || !strings.Contains(reply, "跑步 32 分鐘・5.2 公里") {
		t.Errorf("reply: %s", reply)
	}
}

func TestPlaceNamesStillReachWeather(t *testing.T) {
	f := newFitnessFixture(t, "")
	if f.h.handleFitnessKeyword(context.Background(), "U1", "走馬瀨", "tok") {
		t.Error("place name must not be treated as a workout")
	}
}

// 圖片選單每一格送出的文字都要得到該功能的回覆，而不是被當成地名查天氣.
func TestEveryRichMenuButtonGetsItsFeatureReply(t *testing.T) {
	want := map[string]string{
		"查天氣":  "傳送位置",
		"拍食物":  `"type":"camera"`,
		"記運動":  "做了什麼運動",
		"今日統計": "今日熱量收支",
		"問 AI": "AI 問答目前未啟用",
		"說明":   "我可以幫你",
	}
	for _, button := range richmenu.Buttons() {
		t.Run(button, func(t *testing.T) {
			f := newFitnessFixture(t, "")
			reply := f.say(t, button)
			if strings.Contains(reply, "找不到") || !strings.Contains(reply, want[button]) {
				t.Errorf("button %q got: %s", button, reply)
			}
		})
	}
	if len(richmenu.Buttons()) != len(want) {
		t.Errorf("menu has %d buttons, test covers %d", len(richmenu.Buttons()), len(want))
	}
}

// 沒有啟用運動紀錄時，「查天氣」按鈕仍然有回應.
func TestWeatherButtonWithoutFitness(t *testing.T) {
	api := newLineAPI(t, nil)
	h := NewWebhookHandler(api.client, nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil, conversation.NewMemoryManager())
	h.handleTextMessage(context.Background(), "U1", "查天氣", "tok")
	if reply := api.lastReply(t); !strings.Contains(reply, "傳送位置") {
		t.Errorf("reply: %s", reply)
	}
}
