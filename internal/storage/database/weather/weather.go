package weatherstore

// WeatherForecast 天氣預報儲存結構.
type WeatherForecast struct {
	ID         string            `bson:"_id"              json:"id"`
	City       string            `bson:"city"             json:"city"`
	District   string            `bson:"district"         json:"district"`
	Elements   []ForecastElement `bson:"weatherForecast"  json:"weatherForecast"`
	CreateTime string            `bson:"createTime"       json:"createTime"`
	// Dataset 來源資料集代號（例 F-D0047-003）；啟動補抓判斷用，不進 API 回應；舊文件沒有此欄位 [F30-52].
	Dataset string `bson:"dataset,omitempty" json:"-"`
}

// ForecastElement 天氣預報單一元素.
type ForecastElement struct {
	ElementName string `bson:"elementName" json:"elementName"`
	Description string `bson:"description" json:"description"`
	Value       string `bson:"value"       json:"value"`
	StartTime   string `bson:"startTime"   json:"startTime,omitempty"`
}

// DistrictEntry 代表一筆 city+district 組合.
type DistrictEntry struct {
	City     string `bson:"city"`
	District string `bson:"district"`
}
