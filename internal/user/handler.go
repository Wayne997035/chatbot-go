package user

import (
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/platform/cwa"
	"chatbot-go/internal/redact"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	defaultLimit  = 50
	maxLimit      = 200
	maxOffset     = 100000
	statsTimeout  = 10 * time.Second
	cwaWindowHour = 24
	day           = 24 * time.Hour

	msgBadPaging  = "Invalid limit or offset"
	msgStatsError = "Failed to get stats"
)

var (
	limitPattern  = regexp.MustCompile(`^[0-9]{1,4}$`)
	offsetPattern = regexp.MustCompile(`^[0-9]{1,9}$`)

	errNoStatsStore = errors.New("stats store not configured")
)

// UserHandler 管理員統計處理器（唯讀）.
type UserHandler struct {
	store   StatsStore
	usage   UsageReader
	now     func() time.Time
	timeout time.Duration
}

// NewUserHandler [F30-70] 建立管理員統計處理器；store 為 nil 時請求回 500，usage 為 nil 時回應省略 cwa 區塊.
func NewUserHandler(store StatsStore, usage UsageReader) *UserHandler {
	return &UserHandler{store: store, usage: usage, now: time.Now, timeout: statsTimeout}
}

// GetStats [F30-70] 管理員統計：回應內任何地方都不含完整 LINE userID.
func (h *UserHandler) GetStats(c echo.Context) error {
	limit, offset, ok := parsePaging(c)
	if !ok {
		return c.JSON(http.StatusBadRequest, httputil.ErrorWithCode(httputil.ErrorCodeInvalidParameter, msgBadPaging))
	}
	// 錯誤一律在這裡吞下並回固定訊息，不交給 Echo 全域錯誤處理器 [F30-75].
	resp, err := h.collect(c.Request().Context(), offset, limit)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Error("admin stats failed", "error", redact.Error(err))
		}
		return c.JSON(http.StatusInternalServerError, httputil.ErrorWithCode(httputil.ErrorCodeServerError, msgStatsError))
	}
	return httputil.OK(c, resp)
}

// parsePaging [F30-74] 嚴格解析：只收十進位數字字串，同名參數出現多次即拒絕.
func parsePaging(c echo.Context) (limit, offset int, ok bool) {
	limit, ok = parseParam(c, "limit", limitPattern, defaultLimit, 1, maxLimit)
	if !ok {
		return 0, 0, false
	}
	offset, ok = parseParam(c, "offset", offsetPattern, 0, 0, maxOffset)
	return limit, offset, ok
}

func parseParam(c echo.Context, name string, pattern *regexp.Regexp, def, lo, hi int) (int, bool) {
	vals := c.QueryParams()[name]
	switch len(vals) {
	case 0:
		return def, true
	case 1:
		if !pattern.MatchString(vals[0]) {
			return 0, false
		}
		n, err := strconv.Atoi(vals[0])
		if err != nil || n < lo || n > hi {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// collect 循序查詢；任何一步失敗整個請求失敗，不回部分資料.
func (h *UserHandler) collect(parent context.Context, offset, limit int) (*statsResponse, error) {
	if h.store == nil {
		return nil, errNoStatsStore
	}
	ctx, cancel := context.WithTimeout(parent, h.timeout)
	defer cancel()
	now := h.now()

	resp := &statsResponse{GeneratedAt: now.UTC()}
	if err := h.collectUsers(ctx, now, resp); err != nil {
		return nil, err
	}
	if err := h.collectAlerts(ctx, resp); err != nil {
		return nil, err
	}
	if err := h.collectList(ctx, offset, limit, resp); err != nil {
		return nil, err
	}
	resp.CWA = h.cwaBlock(ctx)
	return resp, nil
}

func (h *UserHandler) collectUsers(ctx context.Context, now time.Time, resp *statsResponse) error {
	var err error
	if resp.Users.Total, err = h.store.CountUsers(ctx); err != nil {
		return err
	}
	// 滾動視窗：起點由這裡算，store 只負責 createTime >= since.
	if resp.Users.JoinedLast7Days, err = h.store.CountUsersCreatedSince(ctx, now.Add(-7*day)); err != nil {
		return err
	}
	if resp.Users.JoinedLast30Days, err = h.store.CountUsersCreatedSince(ctx, now.Add(-30*day)); err != nil {
		return err
	}
	resp.Profiles.Filled, err = h.store.CountFilledProfiles(ctx)
	return err
}

func (h *UserHandler) collectAlerts(ctx context.Context, resp *statsResponse) error {
	var err error
	a := &resp.Alerts
	if a.WeatherWarning.Enabled, err = h.store.CountEnabledByType(ctx, alertsub.TypeWeatherWarning); err != nil {
		return err
	}
	if a.EarthquakeTsunami.Enabled, err = h.store.CountEnabledByType(ctx, alertsub.TypeEarthquakeTsunami); err != nil {
		return err
	}
	if a.AnyEnabled, err = h.store.CountDistinctEnabledUsers(ctx); err != nil {
		return err
	}
	regions, err := h.store.CountWeatherWarningRegions(ctx)
	if err != nil {
		return err
	}
	a.WeatherWarning.Regions = sortedRegions(regions)
	return nil
}

// sortedRegions 不信任 store 的順序：count 降冪、同數量 region 位元組序升冪；不截斷.
func sortedRegions(in []alertsub.RegionCount) []regionCount {
	out := make([]regionCount, 0, len(in))
	for _, r := range in {
		out = append(out, regionCount{Region: r.Region, Count: r.Count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Region < out[j].Region
	})
	return out
}

func (h *UserHandler) collectList(ctx context.Context, offset, limit int, resp *statsResponse) error {
	resp.UserList = userListStats{Total: resp.Users.Total, Limit: limit, Offset: offset, Items: []userItem{}}
	users, err := h.store.ListUsersPage(ctx, offset, limit)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	subs, err := h.store.FindSubscriptionsByUserIDs(ctx, ids)
	if err != nil {
		return err
	}
	byUser := groupAlerts(subs)
	for _, u := range users {
		item := userItem{ID: maskUserID(u.ID), Alerts: byUser[u.ID]}
		if item.Alerts.WeatherWarning.Regions == nil {
			item.Alerts.WeatherWarning.Regions = []string{}
		}
		if !u.CreateTime.IsZero() {
			t := u.CreateTime.UTC()
			item.JoinedAt = &t
		}
		resp.UserList.Items = append(resp.UserList.Items, item)
	}
	return nil
}

// groupAlerts 訂閱依 userID 歸戶；天氣特報的 regions 只取 enabled=true 的那筆.
func groupAlerts(subs []alertsub.AlertSubscription) map[string]userAlerts {
	out := make(map[string]userAlerts, len(subs))
	for _, s := range subs {
		if !s.Enabled {
			continue
		}
		a := out[s.UserID]
		switch s.AlertType {
		case alertsub.TypeWeatherWarning:
			a.WeatherWarning.Enabled = true
			a.WeatherWarning.Regions = append(a.WeatherWarning.Regions, s.Regions...)
		case alertsub.TypeEarthquakeTsunami:
			a.EarthquakeTsunami.Enabled = true
		}
		out[s.UserID] = a
	}
	for id, a := range out {
		sort.Strings(a.WeatherWarning.Regions)
		out[id] = a
	}
	return out
}

// cwaBlock [F30-76] 氣象署用量；讀取器為 nil 時回 nil（省略整個 cwa 鍵）.
// stream 順序與名稱取 cwa.Streams()：缺的補 0、多的丟棄.
func (h *UserHandler) cwaBlock(ctx context.Context) *cwaStats {
	if h.usage == nil {
		return nil
	}
	u := h.usage.Usage(ctx)
	hours := u.WindowHours
	if hours <= 0 {
		hours = cwaWindowHour
	}
	block := &cwaStats{WindowHours: hours, Limit: u.Limit, Used: u.Total, ByStream: make([]streamCalled, 0, len(cwa.Streams()))}
	for _, s := range cwa.Streams() {
		block.ByStream = append(block.ByStream, streamCalled{Stream: s, Calls: u.ByStream[s]})
	}
	return block
}
