package fitness

import (
	"math"
	"testing"
)

// TestParseColloquialDurations [F36-07] 規格 A-RUN 運動口語範例表：預期值取自規格，不取自實作輸出.
func TestParseColloquialDurations(t *testing.T) {
	cases := []struct {
		input    string
		activity string // "" = Activity 為 nil
		minutes  float64
		km       float64
	}{
		{"跑步半小時", "跑步", 30, 0},
		{"跑步半個小時", "跑步", 30, 0},
		{"跑步半個鐘頭", "跑步", 30, 0},
		{"游泳 半個小時三公里", "游泳", 30, 3},
		{"游泳一個小時", "游泳", 60, 0},
		{"游泳一個半小時", "游泳", 90, 0},
		{"跑步兩個半小時", "跑步", 150, 0},
		{"走路一小時半", "快走", 90, 0},
		{"重訓兩小時", "重訓", 120, 0},
		{"跑步一個小時三十分", "跑步", 90, 0},
		{"跑步三十五分鐘", "跑步", 35, 0},
		{"跑步一百分鐘", "跑步", 100, 0},
		{"跑步一百二十分鐘", "跑步", 120, 0},
		{"游泳二點五小時", "游泳", 150, 0},
		{"跑步一百五分鐘", "跑步", 0, 0},
		{"跑步一點五公里", "跑步", 0, 1.5},
		{"今天跑了一點五公里", "跑步", 0, 1.5},
		{"跑步十二點五公里", "跑步", 0, 12.5},
		{"跑步五公里三十分鐘", "跑步", 30, 5},
		{"跑步半小時5公里", "跑步", 30, 5},
		{"游泳一公里", "游泳", 0, 1},
		{"騎車二十公里", "騎車", 0, 20},
		{"騎車一百公里", "騎車", 0, 100},
		{"跑步一千公尺", "跑步", 0, 1.0},
		{"游泳一千公尺", "游泳", 0, 1.0},
		{"跑步一千五百公尺", "跑步", 0, 1.5},
		{"游泳兩千公尺", "游泳", 0, 2.0},
		{"跑步八百公尺", "跑步", 0, 0.8},
		{"下午三點跑步 5公里", "跑步", 0, 5},
		{"打了一場球", "", 0, 0},
		{"跑了一下", "跑步", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			p := Parse(c.input)
			if p == nil {
				t.Fatal("Parse returned nil")
			}
			switch {
			case c.activity == "" && p.Activity != nil:
				t.Errorf("activity = %q, want nil", p.Activity.Name)
			case c.activity != "" && p.Activity == nil:
				t.Errorf("activity = nil, want %q", c.activity)
			case c.activity != "" && p.Activity.Name != c.activity:
				t.Errorf("activity = %q, want %q", p.Activity.Name, c.activity)
			}
			if math.Abs(p.DurationMin-c.minutes) > 1e-9 {
				t.Errorf("minutes = %v, want %v", p.DurationMin, c.minutes)
			}
			if math.Abs(p.DistanceKM-c.km) > 1e-9 {
				t.Errorf("km = %v, want %v", p.DistanceKM, c.km)
			}
		})
	}
}

// TestParseColloquialNeverReturnsLegacyWrongValue [F36-07] 2dbeafd6 五個必過錯例：不得再等於現況的靜默錯值.
func TestParseColloquialNeverReturnsLegacyWrongValue(t *testing.T) {
	cases := []struct {
		input    string
		minutes  *float64 // 不得等於
		distance *float64
	}{
		{"跑步一百二十分鐘", f64(20), nil},
		{"跑步一百五分鐘", f64(5), nil},
		{"游泳二點五小時", f64(300), nil},
		{"跑步一點五公里", nil, f64(5)},
		{"跑步十二點五公里", nil, f64(5)},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			p := Parse(c.input)
			if c.minutes != nil && p.DurationMin == *c.minutes {
				t.Errorf("minutes = %v, the legacy wrong value", p.DurationMin)
			}
			if c.distance != nil && p.DistanceKM == *c.distance {
				t.Errorf("km = %v, the legacy wrong value", p.DistanceKM)
			}
		})
	}
}

func f64(v float64) *float64 { return &v }
