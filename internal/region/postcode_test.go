package region

import (
	"slices"
	"testing"
)

// [F241-13] 郵遞區號查表：唯一一區才回，一碼多區、查不到、格式不對都回 ok=false.
func TestTownshipByPostcode(t *testing.T) {
	tests := []struct {
		name       string
		code       string
		wantCounty string
		wantTown   string
		wantOK     bool
	}{
		{"six digits (jiufen)", "22448", "新北市", "瑞芳區", true},
		{"three digits", "224", "新北市", "瑞芳區", true},
		{"six digits new format", "360023", "苗栗縣", "苗栗市", true},
		{"yangmingshan", "11291", "台北市", "北投區", true},
		{"multi district hsinchu city", "300", "", "", false},
		{"multi district hsinchu city six", "30041", "", "", false},
		{"multi district chiayi city", "600", "", "", false},
		{"not in region list diaoyutai", "290", "", "", false},
		{"unknown code", "999", "", "", false},
		{"empty", "", "", "", false},
		{"too short", "22", "", "", false},
		{"not digits", "ab3", "", "", false},
		{"surrounding spaces", " 224 ", "新北市", "瑞芳區", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			county, town, ok := TownshipByPostcode(tt.code)
			if county != tt.wantCounty || town != tt.wantTown || ok != tt.wantOK {
				t.Fatalf("TownshipByPostcode(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.code, county, town, ok, tt.wantCounty, tt.wantTown, tt.wantOK)
			}
		})
	}
}

// [F241-13] 表內每一區都在鄉鎮清單內（臺字已轉台），清單每個鄉鎮都被至少一個號碼對到.
func TestPostcodeTableMatchesTownships(t *testing.T) {
	covered := make(map[string]bool)
	districts := 0
	var multi []string

	for code, areas := range postcodeTable {
		if len(areas) > 1 {
			multi = append(multi, code)
		}
		for _, a := range areas {
			districts++
			if !IsTownship(a.county, a.town) {
				t.Errorf("code %s: %s%s is not in township list", code, a.county, a.town)
			}
			covered[a.county+a.town] = true
		}
	}

	for _, c := range Counties {
		for _, town := range Townships(c) {
			if !covered[c+town] {
				t.Errorf("%s%s has no postcode", c, town)
			}
		}
	}

	slices.Sort(multi)
	if len(postcodeTable) != 365 {
		t.Errorf("distinct codes = %d, want 365", len(postcodeTable))
	}
	if districts != 368 {
		t.Errorf("districts = %d, want 368", districts)
	}
	if !slices.Equal(multi, []string{"300", "600"}) {
		t.Errorf("multi-district codes = %v, want [300 600]", multi)
	}
}
