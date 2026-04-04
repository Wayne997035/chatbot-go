package weatherstore

// WeatherForecast 天氣預報儲存結構.
type WeatherForecast struct {
	ID         string            `bson:"_id"              json:"id"`
	City       string            `bson:"city"             json:"city"`
	District   string            `bson:"district"         json:"district"`
	Elements   []ForecastElement `bson:"weatherForecast"  json:"weatherForecast"`
	CreateTime string            `bson:"createTime"       json:"createTime"`
}

// ForecastElement 天氣預報單一元素.
type ForecastElement struct {
	ElementName string `bson:"elementName" json:"elementName"`
	Description string `bson:"description" json:"description"`
	Value       string `bson:"value"       json:"value"`
	StartTime   string `bson:"startTime"   json:"startTime,omitempty"`
}
