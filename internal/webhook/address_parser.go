package webhook

import "strings"

// ParseAddress 從地址字串解析城市與區域.
// 支援格式：
//   - "台灣台北市信義區市府路1號" → ("台北市", "信義區")
//   - "台灣新北市板橋區..." → ("新北市", "板橋區")
//   - "台灣臺南縣白河鎮..." → ("臺南縣", "白河鎮")
func ParseAddress(address string) (city, district string) {
	address = strings.ReplaceAll(address, "臺", "台")
	runes := []rune(address)

	cityIdx := indexOfRune(runes, '市')
	countyIdx := indexOfRune(runes, '縣')

	switch {
	case countyIdx >= 2:
		// 縣優先（避免「新竹縣竹北市」被「市」先抓走）
		city = string(runes[countyIdx-2 : countyIdx+1])
		remaining := runes[countyIdx+1:]
		district = extractCountyDistrict(remaining)

	case cityIdx >= 2:
		city = string(runes[cityIdx-2 : cityIdx+1])
		remaining := runes[cityIdx+1:]
		district = extractDistrict(remaining)
	}

	return city, district
}

func extractDistrict(runes []rune) string {
	distIdx := indexOfRune(runes, '區')
	if distIdx >= 1 {
		return string(runes[:distIdx+1])
	}
	return ""
}

func extractCountyDistrict(runes []rune) string {
	for _, suffix := range []rune{'市', '鄉', '鎮', '區'} {
		idx := indexOfRune(runes, suffix)
		if idx >= 1 {
			return string(runes[:idx+1])
		}
	}
	return ""
}

func indexOfRune(runes []rune, target rune) int {
	for i, r := range runes {
		if r == target {
			return i
		}
	}
	return -1
}
