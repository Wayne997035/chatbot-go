package models

// CWA (Central Weather Administration) Open Data API 回應結構.

type CWAResponse struct {
	Success string     `json:"success"`
	Result  CWAResult  `json:"result"`
	Records CWARecords `json:"records"`
}

type CWAResult struct {
	ResourceID string `json:"resource_id"`
}

type CWARecords struct {
	Locations []CWALocations `json:"Locations"`
}

type CWALocations struct {
	DatasetDescription string        `json:"DatasetDescription"`
	LocationsName      string        `json:"LocationsName"`
	DataID             string        `json:"Dataid"`
	Location           []CWALocation `json:"Location"`
}

type CWALocation struct {
	LocationName   string              `json:"LocationName"`
	Geocode        string              `json:"Geocode"`
	Latitude       string              `json:"Latitude"`
	Longitude      string              `json:"Longitude"`
	WeatherElement []CWAWeatherElement `json:"WeatherElement"`
}

type CWAWeatherElement struct {
	ElementName string    `json:"ElementName"`
	Time        []CWATime `json:"Time"`
}

type CWATime struct {
	StartTime    string            `json:"StartTime"`
	EndTime      string            `json:"EndTime"`
	DataTime     string            `json:"DataTime"`
	ElementValue []CWAElementValue `json:"ElementValue"`
}

type CWAElementValue struct {
	Temperature                string `json:"Temperature,omitempty"`
	DewPoint                   string `json:"DewPoint,omitempty"`
	RelativeHumidity           string `json:"RelativeHumidity,omitempty"`
	ApparentTemperature        string `json:"ApparentTemperature,omitempty"`
	ComfortIndex               string `json:"ComfortIndex,omitempty"`
	ComfortIndexDescription    string `json:"ComfortIndexDescription,omitempty"`
	WindSpeed                  string `json:"WindSpeed,omitempty"`
	BeaufortScale              string `json:"BeaufortScale,omitempty"`
	WindDirection              string `json:"WindDirection,omitempty"`
	ProbabilityOfPrecipitation string `json:"ProbabilityOfPrecipitation,omitempty"`
	Weather                    string `json:"Weather,omitempty"`
	WeatherCode                string `json:"WeatherCode,omitempty"`
	WeatherDescription         string `json:"WeatherDescription,omitempty"`
}
