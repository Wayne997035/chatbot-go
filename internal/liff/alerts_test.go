package liff

import (
	"chatbot-go/internal/region"
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/subscription"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// 本檔的 package 層級名字一律 acct 前綴（D6b）. 單號 F30-05（R4 / R5）、F30-06（R7 / R8）.

const (
	acctAlertsPath  = "/api/v1/me/alerts"
	acctWeatherPath = "/api/v1/me/alerts/weather_warning"
	acctQuakePath   = "/api/v1/me/alerts/earthquake_tsunami"
)

func (f *acctFixture) acctSeedSub(userID, alertType string, enabled bool, regions ...string) {
	if regions == nil {
		regions = []string{}
	}
	f.subStore.docs[userID+"_"+alertType] = alertsub.AlertSubscription{
		ID: userID + "_" + alertType, UserID: userID, AlertType: alertType, Regions: regions, Enabled: enabled,
	}
}

func (f *acctFixture) acctStoredRegions(userID, alertType string) ([]string, bool) {
	d, ok := f.subStore.docs[userID+"_"+alertType]
	return d.Regions, ok
}

// A13 / F30-05：GET /me/alerts 只含自己、Enabled、已知型別；weather 在前；沒有訂閱回 [] 不是 null.
func TestAcctListAlerts(t *testing.T) {
	f := acctNewFixture(t)
	f.acctSeedSub("U1", alertsub.TypeEarthquakeTsunami, true)
	f.acctSeedSub("U1", alertsub.TypeWeatherWarning, true, "台北市", "新北市板橋區")
	f.acctSeedSub("U2", alertsub.TypeWeatherWarning, true, "高雄市")
	f.acctSeedSub("U1", "legacy_type", true, "x")
	acctGolden(t, f.do(http.MethodGet, acctAlertsPath, "good-U1", ""), "alerts.json")

	// Enabled=false 的舊資料不出現.
	f.acctSeedSub("U1", alertsub.TypeWeatherWarning, false, "台北市")
	rec := f.do(http.MethodGet, acctAlertsPath, "good-U1", "")
	if strings.Contains(rec.Body.String(), "weather_warning") || !strings.Contains(rec.Body.String(), "earthquake_tsunami") {
		t.Errorf("disabled sub listed or enabled sub missing: %s", rec.Body.String())
	}

	rec = f.do(http.MethodGet, acctAlertsPath, "good-U3", "")
	if got := strings.TrimSpace(rec.Body.String()); got != `{"subscriptions":[]}` {
		t.Errorf("no subscriptions = %s, want []", got)
	}
	if rec := f.do(http.MethodGet, acctAlertsPath+"?userId=U2", "good-U1", ""); strings.Contains(rec.Body.String(), "高雄市") {
		t.Errorf("?userId must be ignored: %s", rec.Body.String())
	}
}

// A33（liff 面）：底層不支援讀取 → 500/5001，不得回空清單；一般儲存錯誤 → 500/5001.
type acctNoListStore struct{ upserts int }

func (s *acctNoListStore) Upsert(context.Context, *alertsub.AlertSubscription) error {
	s.upserts++
	return nil
}
func (s *acctNoListStore) Delete(context.Context, string) error { return nil }

func TestAcctListAlertsUnsupportedStoreIsAnErrorNotEmpty(t *testing.T) {
	f := acctNewFixture(t)
	f.h.WithSubscription(subscription.New(&acctNoListStore{}))
	rec := f.do(http.MethodGet, acctAlertsPath, "good-U1", "")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "subscriptions") {
		t.Errorf("unsupported list must not look like 'nothing enabled': %d %s", rec.Code, rec.Body.String())
	}
}

// A14 / F30-05：DELETE 關閉整類；type 不合法 400 且不刪；冪等；不動別人與另一類.
func TestAcctDeleteAlert(t *testing.T) {
	f := acctNewFixture(t)
	f.acctSeedSub("U1", alertsub.TypeWeatherWarning, true, "台北市")
	f.acctSeedSub("U1", alertsub.TypeEarthquakeTsunami, true)
	f.acctSeedSub("U2", alertsub.TypeWeatherWarning, true, "台北市")

	for _, typ := range []string{"bogus", "weather_warning_x", "U2_weather_warning", "Weather_Warning"} {
		rec := f.do(http.MethodDelete, acctAlertsPath+"/"+typ, "good-U1", "")
		if rec.Code != http.StatusBadRequest || acctErrCode(t, rec) != "2001" {
			t.Errorf("type %q: %d %s", typ, rec.Code, rec.Body.String())
		}
	}
	if f.subStore.deletes != 0 {
		t.Fatalf("invalid types must not delete anything (deletes=%d)", f.subStore.deletes)
	}
	if rec := f.do(http.MethodDelete, acctAlertsPath+"/", "good-U1", ""); rec.Code == http.StatusNoContent {
		t.Errorf("empty type must not succeed: %d", rec.Code)
	}

	for i := range 2 { // 重複刪第二次仍 204
		if rec := f.do(http.MethodDelete, acctWeatherPath, "good-U1", ""); rec.Code != http.StatusNoContent {
			t.Fatalf("delete #%d: %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if _, ok := f.acctStoredRegions("U1", alertsub.TypeWeatherWarning); ok {
		t.Error("U1 weather doc still present")
	}
	if _, ok := f.acctStoredRegions("U1", alertsub.TypeEarthquakeTsunami); !ok {
		t.Error("U1 quake doc must stay")
	}
	if _, ok := f.acctStoredRegions("U2", alertsub.TypeWeatherWarning); !ok {
		t.Error("U2's doc must be untouched")
	}
}

// A22 / A25 / F30-06：開啟天氣特報；GET 立即反映；改地區整份取代；壞輸入不寫入、既有訂閱不被覆蓋.
func TestAcctPutWeatherWarning(t *testing.T) {
	f := acctNewFixture(t)
	rec := f.do(http.MethodPut, acctWeatherPath, "good-U1", `{"regions":["台北市","新北市板橋區"]}`)
	acctGolden(t, rec, "alert_item.json")
	if got, ok := f.acctStoredRegions("U1", alertsub.TypeWeatherWarning); !ok || !slices.Equal(got, []string{"台北市", "新北市板橋區"}) || !f.subStore.docs["U1_weather_warning"].Enabled {
		t.Fatalf("stored %v ok=%v", got, ok)
	}
	acctGolden(t, f.do(http.MethodGet, acctAlertsPath, "good-U1", ""), "alerts_weather_only.json")

	// 改地區：整份取代.
	rec = f.do(http.MethodPut, acctWeatherPath, "good-U1", `{"regions":["新北市板橋區"]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"regions":["新北市板橋區"]`) {
		t.Fatalf("change regions: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := f.acctStoredRegions("U1", alertsub.TypeWeatherWarning); !slices.Equal(got, []string{"新北市板橋區"}) {
		t.Errorf("stored after change = %v, old region must be gone", got)
	}

	before, upserts := slices.Clone(f.subStore.docs["U1_weather_warning"].Regions), f.subStore.upserts
	bad := []string{`{"regions":[]}`, `{}`, `{"regions":["火星市"]}`, `{"regions":["台北市火星區"]}`, `{"regions":[""]}`, `{"regions":[123]}`, `not json`}
	for _, body := range bad {
		rec := f.do(http.MethodPut, acctWeatherPath, "good-U1", body)
		if rec.Code != http.StatusBadRequest || acctErrCode(t, rec) != "2001" {
			t.Errorf("body %q: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if rec := f.do(http.MethodPut, acctWeatherPath, "", `{"regions":["台北市"]}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", rec.Code)
	}
	if f.subStore.upserts != upserts {
		t.Errorf("bad requests wrote to the store (%d -> %d)", upserts, f.subStore.upserts)
	}
	if got, _ := f.acctStoredRegions("U1", alertsub.TypeWeatherWarning); !slices.Equal(got, before) {
		t.Errorf("existing subscription overwritten by a bad request: %v", got)
	}
	if rec := f.do(http.MethodPut, acctAlertsPath+"/U2_weather_warning", "good-U1", `{"regions":["台北市"]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("type carrying another user's id must be 400, got %d", rec.Code)
	}
	if _, ok := f.acctStoredRegions("U2", alertsub.TypeWeatherWarning); ok {
		t.Error("U2 must never be written")
	}
}

// A23：臺→台、去重、整縣吃鄉鎮、順序照 region.Counties；上限套在去重前.
func TestAcctPutRegionsNormalizationAndRawCap(t *testing.T) {
	f := acctNewFixture(t)
	rec := f.do(http.MethodPut, acctWeatherPath, "good-U1", `{"regions":["新北市板橋區","臺北市","台北市大安區","台北市","新北市板橋區"]}`)
	want := []string{"台北市", "新北市板橋區"}
	var item AlertItem
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	stored, _ := f.acctStoredRegions("U1", alertsub.TypeWeatherWarning)
	if !slices.Equal(item.Regions, want) || !slices.Equal(stored, want) {
		t.Errorf("response %v stored %v, want both %v", item.Regions, stored, want)
	}

	raw := func(n int) string {
		b, _ := json.Marshal(map[string][]string{"regions": slices.Repeat([]string{"台北市"}, n)})
		return string(b)
	}
	upserts := f.subStore.upserts
	if rec := f.do(http.MethodPut, acctWeatherPath, "good-U1", raw(401)); rec.Code != http.StatusBadRequest || f.subStore.upserts != upserts {
		t.Errorf("401 raw items: %d upserts %d->%d", rec.Code, upserts, f.subStore.upserts)
	}
	if rec := f.do(http.MethodPut, acctWeatherPath, "good-U1", raw(400)); rec.Code != http.StatusOK {
		t.Errorf("400 raw duplicates must be accepted: %d %s", rec.Code, rec.Body.String())
	}
}

// A24 / F30-06：地震海嘯無 body 或 {}；帶 regions 一律 400；不動別人.
func TestAcctPutEarthquakeTsunami(t *testing.T) {
	f := acctNewFixture(t)
	f.acctSeedSub("U2", alertsub.TypeEarthquakeTsunami, false)
	rec := f.do(http.MethodPut, acctQuakePath, "good-U1", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"alertType":"earthquake_tsunami","regions":[]}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if d := f.subStore.docs["U1_earthquake_tsunami"]; !d.Enabled || len(d.Regions) != 0 || d.Regions == nil {
		t.Errorf("stored %+v", d)
	}
	if f.subStore.docs["U2_earthquake_tsunami"].Enabled {
		t.Error("U2's doc was touched")
	}
	upserts := f.subStore.upserts
	if rec := f.do(http.MethodPut, acctQuakePath, "good-U1", `{"regions":["台北市"]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("regions on quake: %d", rec.Code)
	}
	if rec := f.do(http.MethodPut, acctAlertsPath+"/bogus", "good-U1", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bogus type: %d", rec.Code)
	}
	if f.subStore.upserts != upserts {
		t.Error("rejected requests wrote")
	}
}

// A37：body 嚴格性（DisallowUnknownFields、Content-Type、尾端多餘內容、regions 的 null / 型別）.
func TestAcctPutStrictBody(t *testing.T) {
	cases := []struct {
		name, path, body, contentType string
		want                          int
	}{
		{"1 missing regions", acctWeatherPath, `{}`, "application/json", 400},
		{"2 regions null", acctWeatherPath, `{"regions":null}`, "application/json", 400},
		{"3 null element", acctWeatherPath, `{"regions":["台北市",null]}`, "application/json", 400},
		{"4 unknown field", acctWeatherPath, `{"regions":["台北市"],"extra":1}`, "application/json", 400},
		{"5 text/plain", acctWeatherPath, `{"regions":["台北市"]}`, "text/plain", 400},
		{"6 trailing content", acctWeatherPath, `{"regions":["台北市"]}xyz`, "application/json", 400},
		{"6b second object", acctWeatherPath, `{"regions":["台北市"]}{"regions":["台北市"]}`, "application/json", 400},
		{"6c no content type with body", acctWeatherPath, `{"regions":["台北市"]}`, "", 400},
		{"6d regions not an array", acctWeatherPath, `{"regions":"台北市"}`, "application/json", 400},
		{"6e body is a bare array", acctWeatherPath, `["台北市"]`, "application/json", 400},
		{"6f charset parameter ok", acctWeatherPath, `{"regions":["台北市"]}`, "application/json; charset=utf-8", 200},
		{"7 quake no body", acctQuakePath, ``, "", 200},
		{"8 quake empty object", acctQuakePath, `{}`, "application/json", 200},
		{"9 quake regions empty", acctQuakePath, `{"regions":[]}`, "application/json", 400},
		{"10 quake regions null", acctQuakePath, `{"regions":null}`, "application/json", 400},
		{"11 quake unknown field", acctQuakePath, `{"foo":1}`, "application/json", 400},
		{"12 quake null body", acctQuakePath, `null`, "application/json", 400},
		{"13 oversize body", acctWeatherPath, `{"regions":["` + strings.Repeat("a", maxAlertBodyBytes) + `"]}`, "application/json", 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := acctNewFixture(t)
			f.acctSeedSub("U1", alertsub.TypeWeatherWarning, true, "高雄市")
			rec := f.acctDo(http.MethodPut, tc.path, "good-U1", tc.body, tc.contentType)
			if rec.Code != tc.want {
				t.Fatalf("status %d %s, want %d", rec.Code, rec.Body.String(), tc.want)
			}
			if tc.want == 400 {
				if acctErrCode(t, rec) != "2001" {
					t.Errorf("code = %s, want 2001", acctErrCode(t, rec))
				}
				if got, _ := f.acctStoredRegions("U1", alertsub.TypeWeatherWarning); !slices.Equal(got, []string{"高雄市"}) {
					t.Errorf("existing subscription changed: %v", got)
				}
				if f.subStore.upserts != 0 {
					t.Errorf("rejected request wrote (%d upserts)", f.subStore.upserts)
				}
			}
		})
	}
}

// A26 / A27 / A33（liff 面）：R7 刪除標記防寫回（since = 請求開始時間）與錯誤對應.
func TestAcctPutErasureGuardsAndErrorMapping(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(f *acctFixture)
		wantStatus  int
		wantCode    string
		wantUpserts int
		wantDeletes int
		wantDoc     bool
	}{
		{"erased before write", func(f *acctFixture) { f.probe.erased = true }, 410, "4010", 0, 0, false},
		{"mark older than the request is a legal new subscription", func(f *acctFixture) { f.probe.markedAt = fixedNow.Add(-time.Minute) }, 200, "", 1, 0, true},
		{"erased between check and write", func(f *acctFixture) { f.subStore.afterUpsert = func() { f.probe.erased = true } }, 410, "4010", 1, 1, false},
		{"undo fails", func(f *acctFixture) {
			f.subStore.afterUpsert = func() { f.probe.erased = true }
			f.subStore.deleteErr = errors.New("delete down")
		}, 500, "5002", 1, 1, true},
		{"upsert error with mark hit still undoes", func(f *acctFixture) {
			f.subStore.upsertErr = errors.New("write failed")
			f.subStore.afterUpsert = func() { f.probe.erased = true }
		}, 410, "4010", 1, 1, false},
		{"plain storage error is 500/5001", func(f *acctFixture) { f.subStore.upsertErr = errors.New("write failed") }, 500, "5001", 1, 0, false},
	}
	for _, path := range []string{acctWeatherPath, acctQuakePath} {
		for _, tc := range cases {
			t.Run(path[strings.LastIndex(path, "/")+1:]+"/"+tc.name, func(t *testing.T) {
				f := acctNewFixture(t)
				tc.setup(f)
				body := ""
				if path == acctWeatherPath {
					body = `{"regions":["台北市"]}`
				}
				rec := f.do(http.MethodPut, path, "good-U1", body)
				if rec.Code != tc.wantStatus || (tc.wantCode != "" && acctErrCode(t, rec) != tc.wantCode) {
					t.Fatalf("%d %s, want %d/%s", rec.Code, rec.Body.String(), tc.wantStatus, tc.wantCode)
				}
				if f.subStore.upserts != tc.wantUpserts || f.subStore.deletes != tc.wantDeletes {
					t.Errorf("upserts=%d deletes=%d, want %d/%d", f.subStore.upserts, f.subStore.deletes, tc.wantUpserts, tc.wantDeletes)
				}
				if len(f.subStore.docs) > 0 != tc.wantDoc {
					t.Errorf("doc present = %v, want %v", len(f.subStore.docs) > 0, tc.wantDoc)
				}
				if f.subStore.deletes > 0 && f.subStore.deleteCtxErr != nil {
					t.Errorf("undo ctx canceled: %v", f.subStore.deleteCtxErr)
				}
				wantSince := fixedNow.Truncate(time.Millisecond)
				if f.probe.calls > 0 && !f.probe.since.Equal(wantSince) {
					t.Errorf("since = %v, want the request start time %v", f.probe.since, wantSince)
				}
			})
		}
	}
}

// A28：GET /me/regions 與 R7 驗證同一份 region 資料.
func TestAcctRegionsMatchRegionPackageAndPutValidation(t *testing.T) {
	f := acctNewFixture(t)
	rec := f.do(http.MethodGet, "/api/v1/me/regions", "good-U1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var view RegionsView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Counties) != len(region.Counties) || len(view.Counties) != 22 {
		t.Fatalf("counties = %d, want 22", len(view.Counties))
	}
	for i, c := range view.Counties {
		if c.Name != region.Counties[i] || !slices.Equal(c.Townships, region.Townships(c.Name)) || c.Townships == nil {
			t.Errorf("county %d %q differs from the region package", i, c.Name)
		}
		if strings.Contains(rec.Body.String(), "臺") {
			t.Fatal("R8 must never emit 臺")
		}
		for _, town := range c.Townships {
			if _, _, ok := region.SplitArea(c.Name + town); !ok {
				t.Errorf("%s%s fails SplitArea", c.Name, town)
			}
		}
	}
	// 清單裡某縣的鄉鎮全部送 R7 → 200；清單外字串 → 400；沒 token → 401.
	taipei := view.Counties[0]
	var all []string
	for _, town := range taipei.Townships {
		all = append(all, taipei.Name+town)
	}
	body, _ := json.Marshal(map[string][]string{"regions": all})
	if rec := f.do(http.MethodPut, acctWeatherPath, "good-U1", string(body)); rec.Code != http.StatusOK {
		t.Errorf("listed townships rejected by R7: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.do(http.MethodPut, acctWeatherPath, "good-U1", `{"regions":["台北市不存在區"]}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unlisted string accepted: %d", rec.Code)
	}
	if rec := f.do(http.MethodGet, "/api/v1/me/regions", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", rec.Code)
	}
}

// A29：聊天與 LIFF 讀寫同一份（同一個服務）；liff 正式碼不直接碰訂閱 repo.
func TestAcctChatAndLIFFShareOneSubscriptionService(t *testing.T) {
	f := acctNewFixture(t)
	ctx := context.Background()
	// 「聊天」經同一個服務開啟，LIFF 看得到.
	if err := f.svc.SetWeatherWarningRegions(ctx, "U1", []string{"高雄市鳳山區"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(http.MethodGet, acctAlertsPath, "good-U1", ""); !strings.Contains(rec.Body.String(), "高雄市鳳山區") {
		t.Errorf("LIFF cannot see the chat subscription: %s", rec.Body.String())
	}
	// LIFF 改地區後「聊天」關閉，R4 無該項.
	f.do(http.MethodPut, acctWeatherPath, "good-U1", `{"regions":["台北市"]}`)
	if err := f.svc.Disable(ctx, "U1", alertsub.TypeWeatherWarning); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(http.MethodGet, acctAlertsPath, "good-U1", ""); strings.Contains(rec.Body.String(), "weather_warning") {
		t.Errorf("disabled in chat but still listed: %s", rec.Body.String())
	}
	for _, file := range []string{"alerts.go", "account.go", "month.go", "handler.go"} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"alertSubRepo", "alertsub.AlertSubRepo", "AlertSubRepository"} {
			if strings.Contains(string(b), banned) {
				t.Errorf("%s touches the subscription repo directly (%s)", file, banned)
			}
		}
	}
}

// 沒注入訂閱服務的 handler：通知路由回 503，不執行.
func TestAcctAlertRoutesWithoutServiceAre503(t *testing.T) {
	f := acctNewFixture(t)
	f.h.WithSubscription(nil)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, acctAlertsPath, ""},
		{http.MethodPut, acctWeatherPath, `{"regions":["台北市"]}`},
		{http.MethodDelete, acctWeatherPath, ""},
	} {
		if rec := f.do(c.method, c.path, "good-U1", c.body); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
}
