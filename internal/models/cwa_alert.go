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

// CWACapResponse 有鄉鎮明細的天氣特報（W-C0033-003 豪大雨、-004 低溫、-006 陸上強風，CAP 格式）回應.
// 欄位依 2026-11-05 實際回傳（internal/alert/testdata）.
type CWACapResponse struct {
	Success string        `json:"success"`
	Records CWACapRecords `json:"records"`
}

// CWACapRecords 明細記錄；API 會一直回最後發布的那幾則，含已過期與已解除的.
type CWACapRecords struct {
	Info []CWACapInfo `json:"info"`
}

// CWACapInfo 一則特報.
type CWACapInfo struct {
	Event     string        `json:"event"`
	Urgency   string        `json:"urgency"`
	Headline  string        `json:"headline"`
	Expires   string        `json:"expires"`
	Parameter []CWACapParam `json:"parameter"`
	Area      []CWACapArea  `json:"area"`
}

// CWACapParam 特報參數（alert_title、severity_level…）.
type CWACapParam struct {
	ValueName string `json:"valueName"`
	Value     string `json:"value"`
}

// CWACapArea 影響區域；areaDesc 是「縣市」或「縣市+鄉鎮市區」，用「臺」.
type CWACapArea struct {
	AreaDesc string `json:"areaDesc"`
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
