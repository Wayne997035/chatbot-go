package cwa

import (
	"fmt"
	"time"
)

const taipeiName = "Asia/Taipei"

// LoadTaipei 載入 Asia/Taipei（F30-37）. load 為 nil 用 time.LoadLocation.
// 失敗回錯；NEVER 退回 FixedZone 或 UTC（gocron 會把名稱拼進 CRON_TZ，自訂名稱解析不了）.
func LoadTaipei(load func(name string) (*time.Location, error)) (*time.Location, error) {
	if load == nil {
		load = time.LoadLocation
	}
	loc, err := load(taipeiName)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", taipeiName, err)
	}
	if loc == nil {
		return nil, fmt.Errorf("load %s: nil location", taipeiName)
	}
	return loc, nil
}
