package nutrition

import (
	"testing"
	"time"
)

// at 台灣時間 2026-03-04 hh:mm:ss.
func tpAt(h, m, s int) time.Time { return time.Date(2026, 3, 4, h, m, s, 0, Taipei) }

// [F35-01] A1：下午茶時段 [14:00, 18:00) 的邊界.
func TestLabelAtSnackWindowBoundaries(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"13:59", tpAt(13, 59, 0), "點心"},
		{"14:00", tpAt(14, 0, 0), "下午茶"},
		{"15:30", tpAt(15, 30, 0), "下午茶"},
		{"17:59:59", tpAt(17, 59, 59), "下午茶"},
		{"18:00", tpAt(18, 0, 0), "點心"},
		{"00:00", tpAt(0, 0, 0), "點心"},
		{"23:59", tpAt(23, 59, 0), "點心"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Snack.LabelAt(c.at); got != c.want {
				t.Errorf("Snack.LabelAt(%s) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

// [F35-01] A2：小時以台灣時間算，與 UTC、本機時區無關.
func TestLabelAtConvertsToTaiwanTime(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	utc := func(h, m int) time.Time { return time.Date(2026, 3, 4, h, m, 0, 0, time.UTC) }
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"05:59Z = 13:59 Taipei", utc(5, 59), "點心"},
		{"06:00Z = 14:00 Taipei", utc(6, 0), "下午茶"},
		{"09:59Z = 17:59 Taipei", utc(9, 59), "下午茶"},
		{"10:00Z = 18:00 Taipei", utc(10, 0), "點心"},
		{"08:00Z = 16:00 Taipei", utc(8, 0), "下午茶"},
		{"15:00Z = 23:00 Taipei", utc(15, 0), "點心"},
		{"22:30 Los Angeles = 14:30 Taipei next day", time.Date(2026, 3, 3, 22, 30, 0, 0, la), "下午茶"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Snack.LabelAt(c.at); got != c.want {
				t.Errorf("Snack.LabelAt = %q, want %q", got, c.want)
			}
		})
	}
}

// [F35-01] A3：非點心的餐別名稱與時間無關.
func TestLabelAtNonSnackIgnoresTime(t *testing.T) {
	want := map[MealType]string{Breakfast: "早餐", Lunch: "午餐", Dinner: "晚餐", LateNight: "宵夜"}
	for typ, label := range want {
		for h := range 24 {
			if got := typ.LabelAt(tpAt(h, 30, 0)); got != label {
				t.Errorf("%s.LabelAt(%02d:30) = %q, want %q", typ, h, got, label)
			}
		}
	}
}

// [F35-01] A4：未知餐別永遠是「一餐」；零值時間（= 台灣 08:00）的點心顯示「點心」.
func TestLabelAtUnknownTypeAndZeroTime(t *testing.T) {
	for _, typ := range []MealType{"", "brunch"} {
		for _, at := range []time.Time{tpAt(15, 0, 0), tpAt(20, 0, 0)} {
			if got := typ.LabelAt(at); got != "一餐" {
				t.Errorf("%q.LabelAt(%s) = %q, want 一餐", typ, at.In(Taipei).Format("15:04"), got)
			}
		}
	}
	if got := Snack.LabelAt(time.Time{}); got != "點心" {
		t.Errorf("Snack.LabelAt(zero) = %q, want 點心", got)
	}
}

// [F35-01] A5：下午茶的顯示時段不得滲進 MealTypeAt 的猜餐（17 點沒說餐別的正餐仍記晚餐）.
func TestMealTypeAtUnchangedAroundTeaWindow(t *testing.T) {
	cases := []struct {
		h, m int
		want MealType
	}{
		{13, 59, Lunch}, {14, 0, Snack}, {16, 59, Snack}, {17, 0, Dinner}, {17, 59, Dinner}, {18, 0, Dinner},
	}
	for _, c := range cases {
		if got := MealTypeAt(tpAt(c.h, c.m, 0)); got != c.want {
			t.Errorf("MealTypeAt(%02d:%02d) = %q, want %q", c.h, c.m, got, c.want)
		}
	}
}
