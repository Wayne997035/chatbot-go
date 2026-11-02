package nutrition

import "time"

// Taipei 台灣時區；載入失敗時用固定 +8.
var Taipei = loadTaipei()

func loadTaipei() *time.Location {
	if loc, err := time.LoadLocation("Asia/Taipei"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*60*60)
}

// DayRange 回傳 t 所在台灣日期的 [00:00, 隔天 00:00).
func DayRange(t time.Time) (from, to time.Time) {
	local := t.In(Taipei)
	from = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, Taipei)
	return from, from.AddDate(0, 0, 1)
}
