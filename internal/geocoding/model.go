package geocoding

// GeocodeResult 代表一筆地理編碼結果.
type GeocodeResult struct {
	City        string // 台北市 / 南投縣
	District    string // 大安區 / 魚池鄉
	DisplayName string // Nominatim 回傳的完整地名（供顯示用）
	// NeedsConfirm [F241-14] 只來自公車站（highway/bus_stop）的結果：可能是遠處同名站牌，單一候選時要使用者確認.
	NeedsConfirm bool
	// Area [F38-01] 該縣市鄉鎮至少有一筆結果的 OSM category 不屬於點狀交通設施（railway、highway、public_transport、aerialway、amenity）：
	// 代表查詢字是一個區域或景點（天母、日月潭），單打時可以出選項卡；全是車站、站牌時為 false（「幸福」這類一般詞）.
	Area bool
}
