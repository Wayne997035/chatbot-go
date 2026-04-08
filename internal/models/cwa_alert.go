package models

// CWA 災害警報 API 回應結構.
// NOTE: 以下結構依據 CWA Open Data 慣例定義，實際欄位需依 API 回應調整.

// CWAAlertResponse 天氣特報 (W-C0033-001) 回應.
type CWAAlertResponse struct {
	Success string          `json:"success"`
	Records CWAAlertRecords `json:"records"`
}

// CWAAlertRecords 天氣特報記錄.
type CWAAlertRecords struct {
	Location []CWAAlertLocation `json:"location"`
}

// CWAAlertLocation 天氣特報地區.
type CWAAlertLocation struct {
	LocationName    string        `json:"locationName"`
	HazardCondition CWAHazardCond `json:"hazardConditions"`
}

// CWAHazardCond 危害條件.
type CWAHazardCond struct {
	Hazards []CWAHazard `json:"hazards"`
}

// CWAHazard 危害資訊.
type CWAHazard struct {
	Info CWAHazardInfo `json:"info"`
}

// CWAHazardInfo 危害細節.
type CWAHazardInfo struct {
	Phenomena    string `json:"phenomena"`
	Significance string `json:"significance"`
}

// CWAEarthquakeResponse 地震報告 (E-A0015-001) 回應.
type CWAEarthquakeResponse struct {
	Success string               `json:"success"`
	Records CWAEarthquakeRecords `json:"records"`
}

// CWAEarthquakeRecords 地震記錄.
type CWAEarthquakeRecords struct {
	Earthquake []CWAEarthquake `json:"Earthquake"`
}

// CWAEarthquake 地震資料.
type CWAEarthquake struct {
	EarthquakeNo   int               `json:"EarthquakeNo"`
	ReportType     string            `json:"ReportType"`
	ReportContent  string            `json:"ReportContent"`
	ReportImageURI string            `json:"ReportImageURI"`
	EarthquakeInfo CWAEarthquakeInfo `json:"EarthquakeInfo"`
}

// CWAEarthquakeInfo 地震詳細資訊.
type CWAEarthquakeInfo struct {
	OriginTime string       `json:"OriginTime"`
	FocalDepth float64      `json:"FocalDepth"`
	EpiCenter  CWAEpiCenter `json:"Epicenter"`
	Magnitude  CWAMagnitude `json:"EarthquakeMagnitude"`
}

// CWAEpiCenter 震央.
type CWAEpiCenter struct {
	Location string `json:"Location"`
}

// CWAMagnitude 規模.
type CWAMagnitude struct {
	Value float64 `json:"MagnitudeValue"`
}

// CWATsunamiResponse 海嘯警報 (E-A0014-001) 回應.
type CWATsunamiResponse struct {
	Success string            `json:"success"`
	Records CWATsunamiRecords `json:"records"`
}

// CWATsunamiRecords 海嘯記錄.
type CWATsunamiRecords struct {
	Tsunami []CWATsunami `json:"Tsunami"`
}

// CWATsunami 海嘯資料.
type CWATsunami struct {
	TsunamiNo      int               `json:"TsunamiNo"`
	ReportType     string            `json:"ReportType"`
	ReportContent  string            `json:"ReportContent"`
	ReportNo       string            `json:"ReportNo"`
	ReportColor    string            `json:"ReportColor"`
	Web            string            `json:"Web"`
	EarthquakeInfo CWAEarthquakeInfo `json:"EarthquakeInfo"`
}
