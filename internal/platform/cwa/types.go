// Package cwa 氣象署共用層：HTTP 客戶端（重試、型別化錯誤、回應上限）、用量記帳與額度判斷.
// 警報端與預報端共用同一份 Ledger；各自建一個 Client（每次嘗試期限不同）.
package cwa

import (
	"errors"
	"fmt"
	"time"
)

// 用量記帳的五個 stream（F30-33；統計 API 不自行定義字串）.
const (
	StreamWarning    = "warning"
	StreamEarthquake = "earthquake"
	StreamTsunami    = "tsunami"
	StreamCold       = "cold"
	StreamForecast   = "forecast"
)

// Streams 固定順序的 stream 清單.
func Streams() []string {
	return []string{StreamWarning, StreamEarthquake, StreamTsunami, StreamCold, StreamForecast}
}

// ForecastDatasetCount 預報每次同步的份數（規格 T10）；預報組的資料集清單長度必須等於它.
const ForecastDatasetCount = 23

// 每次嘗試的期限、重試等待與回應上限.
const (
	AlertAttemptTimeout    = 10 * time.Second
	ForecastAttemptTimeout = 30 * time.Second
	RetryBackoff           = time.Second
	MaxBody                = 10 << 20
)

// ErrQuotaReserved 額度預留判斷不通過（或暫停中），該次不發請求.
var ErrQuotaReserved = errors.New("cwa: quota reserved")

// Error 抓取失敗的型別化錯誤（F30-32）：只輸出分類與次數，不含內層訊息（避免位址、主機名外洩）.
type Error struct {
	Class    string // dns | tls | timeout | conn_reset | network | status_<code> | decode | too_large
	Attempts int
	Err      error
}

// Error 恰為 "cwa fetch failed: <class> (<n> attempts)".
func (e *Error) Error() string {
	return fmt.Sprintf("cwa fetch failed: %s (%d attempts)", e.Class, e.Attempts)
}

// Unwrap 回內層原因，使 errors.Is 能穿透（例如 context.DeadlineExceeded、ErrQuotaReserved）.
func (e *Error) Unwrap() error { return e.Err }

// CallSpec 每次呼叫的屬性.
type CallSpec struct {
	Stream  string // Stream* 之一.
	Exempt  bool   // true = 第一次嘗試不經額度判斷（規格 T26）；重試一律經判斷.
	Dataset string // 資料集代號，只用於 log 的 dataset 欄位（例 "W-C0033-001"）.
}

// Usage 過去 24 小時的氣象署呼叫用量（唯讀）.
type Usage struct {
	Total       int
	ByStream    map[string]int
	Limit       int
	WindowHours int
}
