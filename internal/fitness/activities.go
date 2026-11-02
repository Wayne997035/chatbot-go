// Package fitness 運動紀錄解析與熱量計算.
//
// 熱量公式：大卡 = MET × 體重(kg) × 時間(小時)，MET 取自 Compendium of Physical Activities.
package fitness

import (
	"sort"
	"strings"
)

// Activity 一種運動.
type Activity struct {
	Name string
	MET  float64
	// Aliases 使用者可能的說法（小寫）.
	Aliases []string
	// speedMET 依時速（km/h）決定 MET，由慢到快；nil 表示固定 MET.
	speedMET []speedBand
}

type speedBand struct {
	maxKMH float64
	met    float64
}

// 運動名稱.
const (
	ActivityRunning = "跑步"
	ActivityWalking = "快走"
	ActivityCycling = "騎車"
	ActivityOther   = "其他運動"
)

// activities 常見運動；Aliases 越長越先比對.
var activities = []*Activity{
	{
		Name: ActivityRunning, MET: 8.3,
		Aliases:  []string{"跑步", "慢跑", "馬拉松", "半馬", "全馬", "路跑", "晨跑", "夜跑", "跑", "jogging", "jog", "running", "run"},
		speedMET: []speedBand{{7, 6.0}, {8.5, 8.3}, {10, 9.8}, {11.5, 11.0}, {13, 11.8}, {15, 12.8}, {99, 14.5}},
	},
	{
		Name: ActivityWalking, MET: 4.3, Aliases: []string{"快走", "健走", "走路", "散步", "步行", "走", "walking", "walk"},
		speedMET: []speedBand{{4, 3.0}, {5.5, 3.5}, {6.5, 4.3}, {99, 5.0}},
	},
	{
		Name: ActivityCycling, MET: 7.5, Aliases: []string{"騎腳踏車", "騎單車", "騎自行車", "腳踏車", "自行車", "單車", "騎車", "cycling", "bike"},
		speedMET: []speedBand{{16, 4.0}, {19, 6.8}, {22, 8.0}, {25, 10.0}, {99, 12.0}},
	},
	{Name: "飛輪", MET: 8.5, Aliases: []string{"飛輪", "spinning", "室內腳踏車"}},
	{Name: "游泳", MET: 6.0, Aliases: []string{"游泳", "游", "swimming", "swim"}},
	{Name: "重訓", MET: 5.0, Aliases: []string{"重量訓練", "重訓", "健身", "舉重", "練腿", "練胸", "練背", "gym", "weights"}},
	{Name: "瑜珈", MET: 2.5, Aliases: []string{"瑜珈", "瑜伽", "yoga"}},
	{Name: "皮拉提斯", MET: 3.0, Aliases: []string{"皮拉提斯", "pilates"}},
	{Name: "跳繩", MET: 11.0, Aliases: []string{"跳繩"}},
	{Name: "爬山", MET: 6.0, Aliases: []string{"爬山", "登山", "健行", "hiking"}},
	{Name: "爬樓梯", MET: 8.8, Aliases: []string{"爬樓梯", "爬樓"}},
	{Name: "籃球", MET: 6.5, Aliases: []string{"籃球", "打球", "basketball"}},
	{Name: "羽球", MET: 5.5, Aliases: []string{"羽球", "羽毛球", "badminton"}},
	{Name: "網球", MET: 7.3, Aliases: []string{"網球", "tennis"}},
	{Name: "桌球", MET: 4.0, Aliases: []string{"桌球", "乒乓球"}},
	{Name: "足球", MET: 7.0, Aliases: []string{"足球", "soccer"}},
	{Name: "排球", MET: 4.0, Aliases: []string{"排球"}},
	{Name: "有氧", MET: 7.3, Aliases: []string{"有氧舞蹈", "有氧運動", "有氧", "zumba", "跳舞", "舞蹈"}},
	{Name: "拳擊", MET: 7.8, Aliases: []string{"拳擊", "泰拳", "boxing"}},
	{Name: "HIIT", MET: 8.0, Aliases: []string{"hiit", "間歇", "tabata", "高強度"}},
	{Name: "划船", MET: 7.0, Aliases: []string{"划船機", "划船", "rowing"}},
	{Name: "伸展", MET: 2.3, Aliases: []string{"伸展", "拉筋", "stretching"}},
}

// otherActivity 認不出種類時使用的中等強度.
var otherActivity = &Activity{Name: ActivityOther, MET: 5.0}

type alias struct {
	text     string
	activity *Activity
}

// aliasIndex 依長度由長到短，避免「跑」先吃掉「跑步機」之類較長的說法.
var aliasIndex = buildAliasIndex()

func buildAliasIndex() []alias {
	var out []alias
	for _, a := range activities {
		for _, s := range a.Aliases {
			out = append(out, alias{text: s, activity: a})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].text) > len(out[j].text) })
	return out
}

// FindActivity 在文字中找出運動種類.
func FindActivity(text string) *Activity {
	lower := strings.ToLower(text)
	for _, a := range aliasIndex {
		if strings.Contains(lower, a.text) {
			return a.activity
		}
	}
	return nil
}

// ActivityByName 依名稱或別名找運動；找不到回傳「其他運動」.
func ActivityByName(name string) *Activity {
	if a := FindActivity(name); a != nil {
		return a
	}
	return otherActivity
}

// metFor 依速度調整 MET.
func (a *Activity) metFor(distanceKM, durationMin float64) float64 {
	if len(a.speedMET) == 0 || distanceKM <= 0 || durationMin <= 0 {
		return a.MET
	}
	speed := distanceKM / (durationMin / 60)
	for _, band := range a.speedMET {
		if speed <= band.maxKMH {
			return band.met
		}
	}
	return a.MET
}

// QuickActivities 「記運動」快速按鈕顯示的運動.
var QuickActivities = []string{ActivityRunning, ActivityWalking, ActivityCycling, "重訓", "游泳", "瑜珈", "羽球", "HIIT"}
