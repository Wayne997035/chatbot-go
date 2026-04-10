package weather

import (
	"strings"

	weatherstore "chatbot-go/internal/storage/database/weather"
)

// DistrictEntry is an alias for the shared type in weatherstore.
// Kept here so callers in this package can use weather.DistrictEntry directly.
type DistrictEntry = weatherstore.DistrictEntry

// adminSuffixes are the trailing characters stripped when building a
// suffix-free combined key for city+district matching (e.g. "台北市大安區" → "台北大安").
var adminSuffixes = []string{"市", "縣", "區", "鄉", "鎮", "村", "里"}

// stripAdminSuffix removes a single trailing administrative suffix from s.
func stripAdminSuffix(s string) string {
	for _, suf := range adminSuffixes {
		if strings.HasSuffix(s, suf) {
			return s[:len(s)-len(suf)]
		}
	}
	return s
}

// normalizeQuery normalises a query string for fuzzy matching:
// replaces "臺" with "台", trims whitespace, and lowercases.
func normalizeQuery(s string) string {
	s = strings.ReplaceAll(s, "臺", "台")
	s = strings.TrimSpace(s)
	return strings.ToLower(s)
}

// FuzzyMatch finds all DistrictEntry candidates that contain query as a
// substring (after normalisation). Three forms are checked per candidate:
//  1. district name only
//  2. city+district concatenation (full)
//  3. city+district with administrative suffixes stripped (e.g. "台北大安"
//     matches "台北市大安區")
//
// Duplicates (same city+district) are removed; original casing of
// City/District is preserved in the returned slice.
func FuzzyMatch(candidates []DistrictEntry, query string) []DistrictEntry {
	normQuery := normalizeQuery(query)
	if normQuery == "" {
		return nil
	}

	seen := make(map[string]struct{})
	var results []DistrictEntry

	for _, c := range candidates {
		normDistrict := normalizeQuery(c.District)
		normCombined := normalizeQuery(c.City + c.District)
		normStripped := normalizeQuery(stripAdminSuffix(c.City) + stripAdminSuffix(c.District))

		matched := strings.Contains(normDistrict, normQuery) ||
			strings.Contains(normCombined, normQuery) ||
			strings.Contains(normStripped, normQuery)

		if matched {
			key := c.City + "\x00" + c.District
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			results = append(results, c)
		}
	}

	return results
}
