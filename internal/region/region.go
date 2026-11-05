// Package region 台灣縣市與鄉鎮市區清單（名稱一律用「台」），供訂閱選單與特報比對共用.
package region

import (
	"slices"
	"strings"
)

// Counties 台灣 22 縣市；順序就是選單按鈕的順序.
var Counties = []string{
	"台北市", "新北市", "桃園市", "台中市", "台南市", "高雄市",
	"基隆市", "新竹市", "嘉義市", "新竹縣", "苗栗縣", "彰化縣",
	"南投縣", "雲林縣", "嘉義縣", "屏東縣", "宜蘭縣", "花蓮縣",
	"台東縣", "澎湖縣", "金門縣", "連江縣",
}

// IsCounty 是否為 22 縣市之一（要先把「臺」換成「台」）.
func IsCounty(s string) bool {
	return slices.Contains(Counties, s)
}

// Townships 該縣市的鄉鎮市區（複本）；不認得的縣市回 nil.
func Townships(county string) []string {
	return slices.Clone(townships[county])
}

// IsTownship 鄉鎮市區是否屬於該縣市.
func IsTownship(county, town string) bool {
	return slices.Contains(townships[county], town)
}

// SplitArea 把「縣市+鄉鎮市區」拆開；不是清單內的組合時 ok 為 false.
// [F24-98] 逐一比對縣市名稱的前綴，不依賴縣市名稱的字數.
func SplitArea(s string) (county, town string, ok bool) {
	for _, c := range Counties {
		if rest, found := strings.CutPrefix(s, c); found && IsTownship(c, rest) {
			return c, rest, true
		}
	}
	return "", "", false
}

// Areas 縣市層級特報要比對的訂閱地區：整個縣市，加上該縣每個「縣市+鄉鎮市區」.
// [F24-98] 不認得的縣市只回它自己，照舊通知訂閱整個縣市的人.
func Areas(county string) []string {
	towns := townships[county]
	out := make([]string, 0, 1+len(towns))
	out = append(out, county)
	for _, t := range towns {
		out = append(out, county+t)
	}
	return out
}
