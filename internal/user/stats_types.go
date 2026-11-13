package user

import (
	"regexp"
	"time"
)

// 回應專用型別 [F30-71]：NEVER 直接序列化 userstore.User / alertsub.AlertSubscription / profile.Profile，
// 避免完整 userID（含 "{userID}_{type}" 形式的訂閱 id）跟著外洩.

type statsResponse struct {
	GeneratedAt time.Time     `json:"generatedAt"`
	Users       usersStats    `json:"users"`
	Profiles    profilesStats `json:"profiles"`
	CWA         *cwaStats     `json:"cwa,omitempty"`
	Alerts      alertsStats   `json:"alerts"`
	UserList    userListStats `json:"userList"`
}

type usersStats struct {
	Total            int64 `json:"total"`
	JoinedLast7Days  int64 `json:"joinedLast7Days"`
	JoinedLast30Days int64 `json:"joinedLast30Days"`
}

type profilesStats struct {
	Filled int64 `json:"filled"`
}

type cwaStats struct {
	WindowHours int            `json:"windowHours"`
	Limit       int            `json:"limit"`
	Used        int            `json:"used"`
	ByStream    []streamCalled `json:"byStream"`
}

type streamCalled struct {
	Stream string `json:"stream"`
	Calls  int    `json:"calls"`
}

type alertsStats struct {
	WeatherWarning    weatherWarningStats `json:"weatherWarning"`
	EarthquakeTsunami enabledStats        `json:"earthquakeTsunami"`
	AnyEnabled        int64               `json:"anyEnabled"`
}

type weatherWarningStats struct {
	Enabled int64         `json:"enabled"`
	Regions []regionCount `json:"regions"`
}

type enabledStats struct {
	Enabled int64 `json:"enabled"`
}

type regionCount struct {
	Region string `json:"region"`
	Count  int64  `json:"count"`
}

type userListStats struct {
	Total  int64      `json:"total"`
	Limit  int        `json:"limit"`
	Offset int        `json:"offset"`
	Items  []userItem `json:"items"`
}

type userItem struct {
	ID       string     `json:"id"` // 一律經 maskUserID
	JoinedAt *time.Time `json:"joinedAt"`
	Alerts   userAlerts `json:"alerts"`
}

type userAlerts struct {
	WeatherWarning    userWeatherWarning `json:"weatherWarning"`
	EarthquakeTsunami userEnabled        `json:"earthquakeTsunami"`
}

type userWeatherWarning struct {
	Enabled bool     `json:"enabled"`
	Regions []string `json:"regions"`
}

type userEnabled struct {
	Enabled bool `json:"enabled"`
}

// lineUserIDPattern LINE userID 的形狀：U + 32 位小寫 16 進位.
var lineUserIDPattern = regexp.MustCompile(`^U[0-9a-f]{32}$`)

// maskUserID [F30-73] 回應內所有 ID 的唯一出口：符合 LINE userID 形狀 → 前 3 + "***" + 後 3（以 rune 計）；其他任何字串 → "***".
func maskUserID(id string) string {
	if !lineUserIDPattern.MatchString(id) {
		return "***"
	}
	r := []rune(id)
	return string(r[:3]) + "***" + string(r[len(r)-3:])
}
