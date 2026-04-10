package weather

import (
	"testing"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

var testCandidates = []weatherstore.DistrictEntry{
	{City: "台北市", District: "大安區"},
	{City: "台北市", District: "中正區"},
	{City: "台北市", District: "信義區"},
	{City: "新北市", District: "板橋區"},
	{City: "新北市", District: "中和區"},
	{City: "臺中市", District: "大里區"},
	{City: "台南市", District: "安南區"},
}

func TestFuzzyMatch(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantCities []string
		wantLen    int
	}{
		{
			name:       "exact district substring match",
			query:      "大安",
			wantLen:    1,
			wantCities: []string{"台北市"},
		},
		{
			name:       "臺 normalized to 台 - city match",
			query:      "臺中",
			wantLen:    1,
			wantCities: []string{"臺中市"},
		},
		{
			name:       "city+district combined match",
			query:      "台北大安",
			wantLen:    1,
			wantCities: []string{"台北市"},
		},
		{
			name:    "partial city match returns multiple",
			query:   "台北",
			wantLen: 3,
		},
		{
			name:    "empty query returns nil",
			query:   "",
			wantLen: 0,
		},
		{
			name:    "no match returns empty",
			query:   "高雄",
			wantLen: 0,
		},
		{
			name:    "whitespace only query returns nil",
			query:   "   ",
			wantLen: 0,
		},
		{
			name:       "district exact match",
			query:      "中正",
			wantLen:    1,
			wantCities: []string{"台北市"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FuzzyMatch(testCandidates, tt.query)
			if len(got) != tt.wantLen {
				t.Errorf("FuzzyMatch(%q) returned %d results, want %d", tt.query, len(got), tt.wantLen)
			}
			if len(tt.wantCities) > 0 {
				for i, wantCity := range tt.wantCities {
					if i >= len(got) {
						t.Errorf("FuzzyMatch(%q) result[%d] missing, want city %q", tt.query, i, wantCity)
						continue
					}
					if got[i].City != wantCity {
						t.Errorf("FuzzyMatch(%q) result[%d].City = %q, want %q", tt.query, i, got[i].City, wantCity)
					}
				}
			}
		})
	}
}

func TestFuzzyMatchDedup(t *testing.T) {
	dupes := []weatherstore.DistrictEntry{
		{City: "台北市", District: "大安區"},
		{City: "台北市", District: "大安區"}, // duplicate
	}
	got := FuzzyMatch(dupes, "大安")
	if len(got) != 1 {
		t.Errorf("FuzzyMatch dedup: got %d results, want 1", len(got))
	}
}

func TestFuzzyMatchPreservesOriginalCase(t *testing.T) {
	// 原始 City 含 "臺" 應保留，不被正規化改變
	candidates := []weatherstore.DistrictEntry{
		{City: "臺中市", District: "大里區"},
	}
	got := FuzzyMatch(candidates, "臺中大里")
	if len(got) != 1 {
		t.Fatalf("FuzzyMatch: got %d results, want 1", len(got))
	}
	if got[0].City != "臺中市" {
		t.Errorf("FuzzyMatch should preserve original City casing, got %q", got[0].City)
	}
	if got[0].District != "大里區" {
		t.Errorf("FuzzyMatch should preserve original District casing, got %q", got[0].District)
	}
}
