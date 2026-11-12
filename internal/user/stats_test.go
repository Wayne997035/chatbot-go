package user

import (
	"bytes"
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/platform/cwa"
	"chatbot-go/internal/storage/database/alertsub"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	userstore "chatbot-go/internal/storage/database/user"
)

// statsCall 經真的 Echo 路由打 handler；errHandled 計數 Echo 全域錯誤處理器被呼叫的次數.
func statsCall(t *testing.T, h *UserHandler, query string) (rec *httptest.ResponseRecorder, errHandled int) {
	t.Helper()
	return statsCallCtx(t, context.Background(), h, query)
}

func statsCallCtx(t *testing.T, ctx context.Context, h *UserHandler, query string) (*httptest.ResponseRecorder, int) {
	t.Helper()
	e := echo.New()
	handled := 0
	e.HTTPErrorHandler = func(err error, c echo.Context) {
		handled++
		httputil.ErrorHandler(err, c)
	}
	e.GET("/stats", h.GetStats)
	url := "/stats"
	if query != "" {
		url += "?" + query
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, url, nil))
	return rec, handled
}

func newTestHandler(store StatsStore, usage UsageReader) *UserHandler {
	h := NewUserHandler(store, usage)
	h.now = func() time.Time { return fixedNow }
	return h
}

func statsOK(t *testing.T, h *UserHandler, query string) (statsDecoded, string) {
	t.Helper()
	rec, _ := statsCall(t, h, query)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /stats?%s = %d, body=%s", query, rec.Code, rec.Body.String())
	}
	var out statsDecoded
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out, rec.Body.String()
}

// statsDecoded 測試端自己的解碼型別（不重用被測的回應型別）.
type statsDecoded struct {
	GeneratedAt string `json:"generatedAt"`
	Users       struct {
		Total, JoinedLast7Days, JoinedLast30Days int64
	} `json:"users"`
	Profiles struct{ Filled int64 } `json:"profiles"`
	CWA      *struct {
		WindowHours int `json:"windowHours"`
		Limit       int `json:"limit"`
		Used        int `json:"used"`
		ByStream    []struct {
			Stream string `json:"stream"`
			Calls  int    `json:"calls"`
		} `json:"byStream"`
	} `json:"cwa"`
	Alerts struct {
		WeatherWarning struct {
			Enabled int64 `json:"enabled"`
			Regions []struct {
				Region string `json:"region"`
				Count  int64  `json:"count"`
			} `json:"regions"`
		} `json:"weatherWarning"`
		EarthquakeTsunami struct{ Enabled int64 } `json:"earthquakeTsunami"`
		AnyEnabled        int64                   `json:"anyEnabled"`
	} `json:"alerts"`
	UserList struct {
		Total, Limit, Offset int
		Items                []statsItem `json:"items"`
	} `json:"userList"`
}

type statsItem struct {
	ID       string  `json:"id"`
	JoinedAt *string `json:"joinedAt"`
	Alerts   struct {
		WeatherWarning struct {
			Enabled bool     `json:"enabled"`
			Regions []string `json:"regions"`
		} `json:"weatherWarning"`
		EarthquakeTsunami struct{ Enabled bool } `json:"earthquakeTsunami"`
	} `json:"alerts"`
}

func usersAt(times ...time.Time) []userstore.User {
	out := make([]userstore.User, 0, len(times))
	for i, ts := range times {
		out = append(out, userstore.User{ID: uid(i + 1), CreateTime: ts})
	}
	return out
}

// F30-71
func TestTotalsMatchStore(t *testing.T) {
	for _, n := range []int{0, 1, 7, 200, 201} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			times := make([]time.Time, n)
			for i := range times {
				times[i] = fixedNow.Add(-time.Duration(i) * time.Hour)
			}
			out, _ := statsOK(t, newTestHandler(&fakeStore{users: usersAt(times...)}, nil), "")
			if out.Users.Total != int64(n) || out.UserList.Total != n {
				t.Errorf("total = %d / %d, want %d", out.Users.Total, out.UserList.Total, n)
			}
			if want := min(n, 50); len(out.UserList.Items) != want {
				t.Errorf("items = %d, want %d", len(out.UserList.Items), want)
			}
		})
	}
}

// F30-71 滾動視窗：起點由 handler 以注入時鐘算；邊界 >=.
func TestJoinedWindows(t *testing.T) {
	cases := []struct {
		name          string
		ages          []time.Duration
		want7, want30 int64
	}{
		{"spec four users", []time.Duration{6*day + 23*time.Hour, 7*day + time.Hour, 29 * day, 31 * day}, 1, 3},
		{"exactly 7d and 30d are inside", []time.Duration{7 * day, 30 * day}, 1, 2},
		{"just outside", []time.Duration{7*day + time.Second, 30*day + time.Second}, 0, 1},
		{"now-7d1h is outside the 7d window", []time.Duration{7*day + time.Hour}, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			times := make([]time.Time, 0, len(tc.ages))
			for _, a := range tc.ages {
				times = append(times, fixedNow.Add(-a))
			}
			out, _ := statsOK(t, newTestHandler(&fakeStore{users: usersAt(times...)}, nil), "")
			if out.Users.JoinedLast7Days != tc.want7 || out.Users.JoinedLast30Days != tc.want30 {
				t.Errorf("joined = %d / %d, want %d / %d", out.Users.JoinedLast7Days, out.Users.JoinedLast30Days, tc.want7, tc.want30)
			}
		})
	}
}

func alertFixture() *fakeStore {
	return &fakeStore{subs: []alertsub.AlertSubscription{
		weatherSub(uid(1), true, "台北市"),                         // A
		weatherSub(uid(2), true, "台北市", "台北市大安區"),               // B
		quakeSub(uid(3), true),                                  // C
		weatherSub(uid(4), false, "高雄市"),                        // D 關閉
		weatherSub(uid(5), true, "台北市"), quakeSub(uid(5), true), // E 兩類
	}}
}

// F30-71
func TestAlertCounts(t *testing.T) {
	out, _ := statsOK(t, newTestHandler(alertFixture(), nil), "")
	if out.Alerts.WeatherWarning.Enabled != 3 || out.Alerts.EarthquakeTsunami.Enabled != 2 || out.Alerts.AnyEnabled != 4 {
		t.Errorf("weather/quake/any = %d/%d/%d, want 3/2/4",
			out.Alerts.WeatherWarning.Enabled, out.Alerts.EarthquakeTsunami.Enabled, out.Alerts.AnyEnabled)
	}
}

// F30-71 地區分組排序：count 降冪、同數量位元組序；不合併縣市與鄉鎮；重複呼叫順序一致.
func TestRegionCountsOrder(t *testing.T) {
	f := alertFixture()
	f.subs = append(f.subs, weatherSub(uid(6), true, "台南市"), weatherSub(uid(7), true, "台中市"))
	type rc struct {
		Region string
		Count  int64
	}
	want := []rc{{"台北市", 3}, {"台中市", 1}, {"台北市大安區", 1}, {"台南市", 1}}
	for i := range 20 {
		out, _ := statsOK(t, newTestHandler(f, nil), "")
		got := make([]rc, 0, len(out.Alerts.WeatherWarning.Regions))
		for _, r := range out.Alerts.WeatherWarning.Regions {
			got = append(got, rc{r.Region, r.Count})
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: regions = %v, want %v", i, got, want)
		}
	}
}

// F30-71 不截斷：390 組全回.
func TestRegionCountsNotTruncated(t *testing.T) {
	var rows []alertsub.RegionCount
	for i := 389; i >= 0; i-- { // 刻意倒序
		rows = append(rows, alertsub.RegionCount{Region: fmt.Sprintf("R%03d", i), Count: 1})
	}
	out, _ := statsOK(t, newTestHandler(&fakeStore{regionsOut: rows}, nil), "")
	got := out.Alerts.WeatherWarning.Regions
	if len(got) != 390 {
		t.Fatalf("regions = %d, want 390", len(got))
	}
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i].Region < got[j].Region }) || got[0].Region != "R000" {
		t.Errorf("regions not in byte order: first=%q", got[0].Region)
	}
}

// F30-71
func TestProfilesFilledIgnoresEmptyShell(t *testing.T) {
	f := &fakeStore{profiles: []fakeProfile{{weight: 70}, {}, {sex: "female"}}}
	out, _ := statsOK(t, newTestHandler(f, nil), "")
	if out.Profiles.Filled != 2 {
		t.Errorf("filled = %d, want 2 (the shell must not count)", out.Profiles.Filled)
	}
}

// F30-71 空資料：所有陣列欄位是 []，不是 null.
func TestEmptyArraysNotNull(t *testing.T) {
	_, body := statsOK(t, newTestHandler(&fakeStore{}, fakeUsage{}), "")
	for _, want := range []string{`"items":[]`, `"regions":[]`, `"byStream":[{`} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, "null") {
		t.Errorf("empty store must not produce null: %s", body)
	}
	// 有使用者但沒訂閱：每列的 regions 也是 []
	f := &fakeStore{users: usersAt(fixedNow)}
	if _, body := statsOK(t, newTestHandler(f, nil), ""); !strings.Contains(body, `"regions":[]`) {
		t.Errorf("per-user regions must be []: %s", body)
	}
}

func statsWalkKeys(v any, visit func(key string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			visit(k)
			statsWalkKeys(vv, visit)
		}
	case []any:
		for _, vv := range x {
			statsWalkKeys(vv, visit)
		}
	}
}

// F30-71 舊資料有 type 值也不輸出 type / byType.
func TestResponseHasNoTypeField(t *testing.T) {
	f := &fakeStore{users: []userstore.User{
		{ID: uid(1), CreateTime: fixedNow, Type: "user"}, {ID: uid(2), CreateTime: fixedNow}, {ID: uid(3), CreateTime: fixedNow},
	}}
	_, body := statsOK(t, newTestHandler(f, fakeUsage{}), "")
	var generic any
	if err := json.Unmarshal([]byte(body), &generic); err != nil {
		t.Fatal(err)
	}
	statsWalkKeys(generic, func(k string) {
		if k == "type" || k == "byType" {
			t.Errorf("response has forbidden key %q", k)
		}
	})
	if strings.Contains(body, "byType") {
		t.Error("body mentions byType")
	}
}

// F30-71 缺 createTime 的舊文件：計入 total、不計近 N 天、排最後、joinedAt 是 null.
func TestMissingCreateTime(t *testing.T) {
	f := &fakeStore{users: []userstore.User{
		{ID: uid(1)}, // 缺 createTime，id 最小也要排最後
		{ID: uid(2), CreateTime: fixedNow.Add(-3 * day)},
		{ID: uid(3), CreateTime: fixedNow.Add(-40 * day)},
	}}
	out, body := statsOK(t, newTestHandler(f, nil), "")
	if out.Users.Total != 3 || out.Users.JoinedLast7Days != 1 || out.Users.JoinedLast30Days != 1 {
		t.Errorf("total/7/30 = %d/%d/%d, want 3/1/1", out.Users.Total, out.Users.JoinedLast7Days, out.Users.JoinedLast30Days)
	}
	items := out.UserList.Items
	if len(items) != 3 || items[2].ID != maskUserID(uid(1)) || items[2].JoinedAt != nil {
		t.Errorf("missing-createTime user must be last with null joinedAt: %+v", items)
	}
	if !strings.Contains(body, `"joinedAt":null`) || strings.Contains(body, "0001-01-01") {
		t.Errorf("joinedAt must be JSON null, not zero time: %s", body)
	}
	if items[0].JoinedAt == nil || *items[0].JoinedAt != "2026-11-07T08:00:00Z" {
		t.Errorf("joinedAt should be UTC RFC3339: %v", items[0].JoinedAt)
	}
}

// F30-72 翻頁：三頁互斥、聯集 120、排序 createTime 降冪 + _id 升冪.
func TestPagination(t *testing.T) {
	var users []userstore.User
	for i := 1; i <= 120; i++ {
		ts := fixedNow.Add(-time.Duration(i) * time.Hour)
		if i > 110 { // 10 位 createTime 相同，ID 刻意與插入順序相反
			ts = fixedNow.Add(-500 * time.Hour)
		}
		users = append(users, userstore.User{ID: uid(1000 - i), CreateTime: ts})
	}
	sorted := append([]userstore.User(nil), users...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].CreateTime.Equal(sorted[j].CreateTime) {
			return sorted[i].CreateTime.After(sorted[j].CreateTime)
		}
		return sorted[i].ID < sorted[j].ID
	})
	h := newTestHandler(&fakeStore{users: users}, nil)
	var got []string
	for _, q := range []struct {
		query string
		size  int
	}{{"limit=50&offset=0", 50}, {"limit=50&offset=50", 50}, {"limit=50&offset=100", 20}} {
		out, _ := statsOK(t, h, q.query)
		if len(out.UserList.Items) != q.size || out.UserList.Total != 120 {
			t.Fatalf("%s: items=%d total=%d", q.query, len(out.UserList.Items), out.UserList.Total)
		}
		for _, it := range out.UserList.Items {
			got = append(got, it.ID)
		}
	}
	seen := map[string]bool{}
	for i, id := range got {
		if seen[id] {
			t.Fatalf("duplicate %s across pages", id)
		}
		seen[id] = true
		if want := maskUserID(sorted[i].ID); id != want {
			t.Fatalf("position %d = %s, want %s", i, id, want)
		}
	}
	if len(got) != 120 {
		t.Errorf("union = %d, want 120", len(got))
	}
	if out, _ := statsOK(t, h, "offset=500"); len(out.UserList.Items) != 0 {
		t.Errorf("offset past total must return no items")
	}
}

// F30-72 清單的 regions 只取 enabled=true 的天氣特報.
func TestUserListRegionsOnlyFromEnabled(t *testing.T) {
	f := &fakeStore{
		users: usersAt(fixedNow, fixedNow.Add(-time.Hour), fixedNow.Add(-2*time.Hour)),
		subs: []alertsub.AlertSubscription{
			weatherSub(uid(1), true, "台北市大安區", "台北市"), // a
			weatherSub(uid(2), false, "高雄市"),          // b 關閉但文件仍留 regions
			quakeSub(uid(3), true),                    // c
		},
	}
	out, body := statsOK(t, newTestHandler(f, nil), "")
	a, b, c := out.UserList.Items[0], out.UserList.Items[1], out.UserList.Items[2]
	if !a.Alerts.WeatherWarning.Enabled || !reflect.DeepEqual(a.Alerts.WeatherWarning.Regions, []string{"台北市", "台北市大安區"}) {
		t.Errorf("a = %+v", a.Alerts)
	}
	if b.Alerts.WeatherWarning.Enabled || len(b.Alerts.WeatherWarning.Regions) != 0 {
		t.Errorf("b (disabled) must not expose regions: %+v", b.Alerts)
	}
	if c.Alerts.WeatherWarning.Enabled || !c.Alerts.EarthquakeTsunami.Enabled {
		t.Errorf("c = %+v", c.Alerts)
	}
	if strings.Contains(body, "高雄市") {
		t.Errorf("disabled subscription's region leaked into the response: %s", body)
	}
}

// F30-73
func TestMaskUserID(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"line user id", "U0123456789abcdef0123456789abcdef", "U01***def"},
		{"short", "Uabc", "***"},
		{"empty", "", "***"},
		{"11 chars", "U0123456789", "***"},
		{"12 chars", "U01234567890", "***"},
		{"33 chars not hex", "U" + strings.Repeat("g", 32), "***"},
		{"uppercase hex", "U0123456789ABCDEF0123456789ABCDEF", "***"},
		{"group id", "C0123456789abcdef0123456789abcdef", "***"},
		{"room id", "R0123456789abcdef0123456789abcdef", "***"},
		{"hex without U prefix", "00123456789abcdef0123456789abcdef", "***"},
		{"33 chars with newline", "U0123456789abcdef0123456789abcde\n", "***"},
		{"full-width char, 33 runes", "U" + strings.Repeat("a", 31) + "Ａ", "***"},
		{"34 chars", "U0123456789abcdef0123456789abcdef0", "***"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := maskUserID(tc.in)
			if got != tc.want {
				t.Errorf("maskUserID(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("result is not valid UTF-8: %q", got)
			}
		})
	}
}

// F30-73 回應全文不含任何完整 userID（也不含「{userID}_{type}」形式的訂閱 id）.
func TestResponseNeverContainsFullUserID(t *testing.T) {
	ids := []string{
		"U0123456789abcdef0123456789abcdef", "Ufedcba9876543210fedcba9876543210", "U00000000000000000000000000000001",
		"Uaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "U1234567890abcdef1234567890abcdef",
	}
	f := &fakeStore{}
	for i, id := range ids {
		f.users = append(f.users, userstore.User{ID: id, CreateTime: fixedNow.Add(-time.Duration(i) * time.Hour), Type: "user"})
		f.subs = append(f.subs, weatherSub(id, true, "台北市"), quakeSub(id, true))
	}
	_, body := statsOK(t, newTestHandler(f, fakeUsage{}), "limit=200")
	if re := regexp.MustCompile(`(?i)U[0-9a-f]{32}`); re.MatchString(body) {
		t.Errorf("response matches a full-ID pattern: %s", body)
	}
	for _, id := range ids {
		if strings.Contains(body, id) {
			t.Errorf("response contains full ID %s", id)
		}
		if !strings.Contains(body, maskUserID(id)) {
			t.Errorf("response lacks masked form of %s", id)
		}
	}
}

// F30-74
func TestPagingParams(t *testing.T) {
	bad := []string{
		"limit=0", "limit=201", "limit=abc", "limit=", "limit=-1", "limit=%2B5", "limit=1.5", "limit=%205", "limit=00005",
		"limit=1&limit=2", "offset=-1", "offset=100001", "offset=1234567890", "offset=1&offset=2", "offset=abc", "offset=%2B5",
	}
	for _, q := range bad {
		t.Run("bad "+q, func(t *testing.T) {
			rec, handled := statsCall(t, newTestHandler(&fakeStore{}, nil), q)
			if rec.Code != http.StatusBadRequest || handled != 0 {
				t.Fatalf("code=%d handled=%d, want 400", rec.Code, handled)
			}
			var resp httputil.Response
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Status == nil || resp.Status.Code != "2001" {
				t.Errorf("want error code 2001: %s", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "abc") {
				t.Errorf("error body echoes input: %s", rec.Body.String())
			}
		})
	}
	good := []struct {
		query         string
		limit, offset int
	}{
		{"", 50, 0},
		{"limit=200", 200, 0},
		{"limit=1", 1, 0},
		{"limit=0050", 50, 0},
		{"offset=0", 50, 0},
		{"offset=100000", 50, 100000},
		{"limit=7&offset=3", 7, 3},
	}
	for _, tc := range good {
		t.Run("good "+tc.query, func(t *testing.T) {
			out, _ := statsOK(t, newTestHandler(&fakeStore{}, nil), tc.query)
			if out.UserList.Limit != tc.limit || out.UserList.Offset != tc.offset {
				t.Errorf("limit/offset = %d/%d, want %d/%d", out.UserList.Limit, out.UserList.Offset, tc.limit, tc.offset)
			}
		})
	}
}

// 把 slog 預設輸出接到緩衝區，測試結束還原.
func statsCaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// F30-75 StatsStore 每個方法各失敗一次 → 整個請求 500，不回部分資料、不外洩、不經 Echo 全域錯誤處理器.
func TestStoreFailureIs500(t *testing.T) {
	methods := []string{
		"CountUsers", "CountUsersCreatedSince", "ListUsersPage", "CountEnabledByType", "CountDistinctEnabledUsers",
		"CountWeatherWarningRegions", "FindSubscriptionsByUserIDs", "CountFilledProfiles",
	}
	// 介面有幾個方法就要有幾個子案例；而且每個方法在正常請求中真的被呼叫到.
	if n := reflect.TypeFor[StatsStore]().NumMethod(); n != len(methods) {
		t.Fatalf("StatsStore has %d methods, test covers %d", n, len(methods))
	}
	probe := &fakeStore{users: usersAt(fixedNow)}
	statsOK(t, newTestHandler(probe, nil), "")
	called := map[string]bool{}
	for _, m := range probe.calls {
		called[m] = true
	}
	for _, m := range methods {
		if !called[m] {
			t.Fatalf("method %s is never called by a normal request", m)
		}
	}
	for _, m := range methods {
		t.Run(m, func(t *testing.T) {
			logs := statsCaptureLog(t)
			f := &fakeStore{users: usersAt(fixedNow), subs: []alertsub.AlertSubscription{weatherSub(uid(1), true, "台北市")}, failAt: m}
			rec, handled := statsCall(t, newTestHandler(f, nil), "")
			var resp httputil.Response
			if rec.Code != http.StatusInternalServerError || json.Unmarshal(rec.Body.Bytes(), &resp) != nil ||
				resp.Status == nil || resp.Status.Code != "5001" || resp.Status.Message != "Failed to get stats" {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
			if handled != 0 {
				t.Errorf("Echo global error handler called %d times", handled)
			}
			for name, text := range map[string]string{"body": rec.Body.String(), "log": logs.String()} {
				if strings.Contains(text, fakeErrID) || strings.Contains(text, uid(1)) {
					t.Errorf("%s leaks a full ID: %s", name, text)
				}
			}
			if strings.Contains(rec.Body.String(), fakeErrSecret) || strings.Contains(rec.Body.String(), `"users"`) {
				t.Errorf("body leaks error text or partial data: %s", rec.Body.String())
			}
		})
	}
}

// F30-75
func TestNilStoreIs500(t *testing.T) {
	rec, handled := statsCall(t, NewUserHandler(nil, nil), "")
	var resp httputil.Response
	if rec.Code != http.StatusInternalServerError || json.Unmarshal(rec.Body.Bytes(), &resp) != nil ||
		resp.Status == nil || resp.Status.Code != "5001" || handled != 0 {
		t.Fatalf("code=%d handled=%d body=%s", rec.Code, handled, rec.Body.String())
	}
}

// F30-75 逾時 → 500 + 一行 error log；取消 → 不寫 error log；查詢循序.
func TestRequestTimeoutAndCancel(t *testing.T) {
	block := func(_ context.Context, _ string) error { return nil }
	t.Run("timeout", func(t *testing.T) {
		logs := statsCaptureLog(t)
		f := &fakeStore{hook: func(ctx context.Context, method string) error {
			if method == "CountUsers" {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		}}
		h := newTestHandler(f, nil)
		h.timeout = 30 * time.Millisecond
		rec, _ := statsCall(t, h, "")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("timeout code = %d", rec.Code)
		}
		if n := strings.Count(logs.String(), "admin stats failed"); n != 1 {
			t.Errorf("error log lines = %d, want 1: %s", n, logs.String())
		}
	})
	t.Run("cancel", func(t *testing.T) {
		logs := statsCaptureLog(t)
		ctx, cancel := context.WithCancel(context.Background())
		f := &fakeStore{hook: func(ctx context.Context, _ string) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		}}
		statsCallCtx(t, ctx, newTestHandler(f, nil), "")
		if strings.Contains(logs.String(), "admin stats failed") {
			t.Errorf("client disconnect must not write an error log: %s", logs.String())
		}
	})
	t.Run("sequential", func(t *testing.T) {
		f := &fakeStore{users: usersAt(fixedNow), hook: block}
		statsOK(t, newTestHandler(f, nil), "")
		if f.overlapped {
			t.Errorf("store calls overlapped: %v", f.calls)
		}
		if len(f.calls) != 10 {
			t.Errorf("calls = %v", f.calls)
		}
	})
}

func statsUsageFixture() cwa.Usage {
	return cwa.Usage{
		Total: 764, Limit: 1000, WindowHours: 24,
		ByStream: map[string]int{"warning": 288, "earthquake": 288, "tsunami": 48, "cold": 48, "forecast": 92},
	}
}

type statsStream struct {
	Stream string
	Calls  int
}

func statsStreams(out statsDecoded) []statsStream {
	var got []statsStream
	for _, s := range out.CWA.ByStream {
		got = append(got, statsStream{s.Stream, s.Calls})
	}
	return got
}

// F30-76
func TestStatsCWAUsageBlock(t *testing.T) {
	want := []statsStream{{"warning", 288}, {"earthquake", 288}, {"tsunami", 48}, {"cold", 48}, {"forecast", 92}}
	for i := range 20 {
		out, _ := statsOK(t, newTestHandler(&fakeStore{}, fakeUsage{statsUsageFixture()}), "")
		if out.CWA == nil || out.CWA.WindowHours != 24 || out.CWA.Limit != 1000 || out.CWA.Used != 764 {
			t.Fatalf("cwa = %+v", out.CWA)
		}
		if got := statsStreams(out); !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: byStream = %v, want %v", i, got, want)
		}
	}
	u := statsUsageFixture()
	u.Limit = 500
	if out, _ := statsOK(t, newTestHandler(&fakeStore{}, fakeUsage{u}), ""); out.CWA.Limit != 500 {
		t.Errorf("limit must come from the reader: %d", out.CWA.Limit)
	}
}

// F30-76
func TestStatsCWAUsageZero(t *testing.T) {
	out, body := statsOK(t, newTestHandler(&fakeStore{}, fakeUsage{}), "")
	if out.CWA == nil || out.CWA.Used != 0 || len(out.CWA.ByStream) != 5 {
		t.Fatalf("cwa = %+v", out.CWA)
	}
	if out.CWA.WindowHours != 24 { // 讀取器沒給視窗時數（零值）時固定回 24
		t.Errorf("windowHours = %d, want 24", out.CWA.WindowHours)
	}
	for _, s := range out.CWA.ByStream {
		if s.Calls != 0 {
			t.Errorf("stream %s calls = %d", s.Stream, s.Calls)
		}
	}
	if !strings.Contains(body, `"byStream":[{"stream":"warning","calls":0}`) {
		t.Errorf("byStream shape: %s", body)
	}
}

// F30-76 讀取器為 nil → 省略整個 cwa 鍵（不是 null）.
func TestStatsWithoutUsageReaderOmitsCWA(t *testing.T) {
	out, body := statsOK(t, newTestHandler(&fakeStore{users: usersAt(fixedNow)}, nil), "")
	if strings.Contains(body, `"cwa"`) || out.Users.Total != 1 {
		t.Errorf("cwa must be omitted and the rest intact: %s", body)
	}
}

// F30-76 介面只有 Usage 一個方法.
func TestStatsUsageReaderIsReadOnly(t *testing.T) {
	if n := reflect.TypeFor[UsageReader]().NumMethod(); n != 1 {
		t.Fatalf("UsageReader has %d methods, want 1 (Usage only)", n)
	}
	var r UsageReader = fakeUsage{}
	if _, ok := reflect.TypeOf(r).MethodByName("Usage"); !ok {
		t.Fatal("fake lacks Usage")
	}
	statsOK(t, newTestHandler(&fakeStore{}, r), "")
}

// F30-76 缺的 stream 補 0、多的丟棄.
func TestStatsCWAUsageNormalized(t *testing.T) {
	u := cwa.Usage{Total: 5, Limit: 1000, ByStream: map[string]int{"warning": 2, "earthquake": 1, "tsunami": 1, "forecast": 1, "bogus": 9}}
	out, body := statsOK(t, newTestHandler(&fakeStore{}, fakeUsage{u}), "")
	want := []statsStream{{"warning", 2}, {"earthquake", 1}, {"tsunami", 1}, {"cold", 0}, {"forecast", 1}}
	if got := statsStreams(out); !reflect.DeepEqual(got, want) {
		t.Errorf("byStream = %v, want %v", got, want)
	}
	if strings.Contains(body, "bogus") {
		t.Errorf("unknown stream leaked: %s", body)
	}
}
