package models

// Line Webhook 結構
type LineWebHook struct {
	Destination string   `json:"destination"`
	Events      []Events `json:"events"`
}
