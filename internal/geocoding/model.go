package geocoding

// GeocodeResult 代表一筆地理編碼結果.
type GeocodeResult struct {
	City        string // 台北市 / 南投縣
	District    string // 大安區 / 魚池鄉
	DisplayName string // Nominatim 回傳的完整地名（供顯示用）
}
