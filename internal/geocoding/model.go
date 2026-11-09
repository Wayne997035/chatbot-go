package geocoding

// GeocodeResult 代表一筆地理編碼結果.
type GeocodeResult struct {
	City        string // 台北市 / 南投縣
	District    string // 大安區 / 魚池鄉
	DisplayName string // Nominatim 回傳的完整地名（供顯示用）
	// NeedsConfirm [F241-14] 只來自公車站（highway/bus_stop）的結果：可能是遠處同名站牌，單一候選時要使用者確認.
	NeedsConfirm bool
}
