package region

import (
	"slices"
	"strings"
	"testing"
)

// [F24-98] 鄉鎮清單要和內政部國土測繪中心的 368 個鄉鎮市區一致，名稱一律用「台」.
func TestTownshipListMatchesNLSC(t *testing.T) {
	if len(Counties) != 22 {
		t.Fatalf("counties: got %d, want 22", len(Counties))
	}
	if len(townships) != len(Counties) {
		t.Fatalf("township keys: got %d, want %d", len(townships), len(Counties))
	}
	total := 0
	for _, c := range Counties {
		towns, ok := townships[c]
		if !ok {
			t.Errorf("county %q has no township list", c)
			continue
		}
		total += len(towns)
		seen := map[string]bool{}
		for _, town := range towns {
			if seen[town] {
				t.Errorf("%s: duplicate township %q", c, town)
			}
			seen[town] = true
			if strings.Contains(town, "臺") {
				t.Errorf("%s: township %q uses 臺", c, town)
			}
		}
		if strings.Contains(c, "臺") {
			t.Errorf("county %q uses 臺", c)
		}
	}
	if total != 368 {
		t.Errorf("townships: got %d, want 368", total)
	}
	if got := Townships("高雄市"); len(got) != 38 || !slices.Contains(got, "那瑪夏區") {
		t.Errorf("高雄市: %d townships, 那瑪夏區=%v", len(got), slices.Contains(got, "那瑪夏區"))
	}
	if !slices.Contains(Townships("雲林縣"), "台西鄉") {
		t.Error("雲林縣 must contain 台西鄉 (normalized from 臺西鄉)")
	}
}

// [F24-98] 縣市層級的特報要通知整縣與該縣每個鄉鎮的訂閱者.
func TestAreas(t *testing.T) {
	got := Areas("台北市")
	if len(got) != 13 || got[0] != "台北市" || !slices.Contains(got, "台北市中正區") {
		t.Errorf("Areas(台北市) = %v", got)
	}
	if got := Areas("火星市"); !slices.Equal(got, []string{"火星市"}) {
		t.Errorf("Areas(火星市) = %v, want only the county itself", got)
	}
	if c, town, ok := SplitArea("雲林縣台西鄉"); c != "雲林縣" || town != "台西鄉" || !ok {
		t.Errorf("SplitArea(雲林縣台西鄉) = %q %q %v", c, town, ok)
	}
	if _, _, ok := SplitArea("台北市火星區"); ok {
		t.Error("an unknown township must not split")
	}
	if _, _, ok := SplitArea("台北市"); ok {
		t.Error("a county alone is not a county+township")
	}
	if !IsCounty("台北市") || IsCounty("台北") || IsCounty("臺北市") {
		t.Error("IsCounty")
	}
	if !IsTownship("新北市", "石門區") || IsTownship("台北市", "石門區") {
		t.Error("IsTownship")
	}
	// 回傳的是複本：改了不影響清單本身.
	towns := Townships("台北市")
	towns[0] = "改掉"
	if Townships("台北市")[0] == "改掉" {
		t.Error("Townships must return a copy")
	}
}
