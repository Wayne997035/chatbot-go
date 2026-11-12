package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// [F303-11] The fake server must hand the page the shared golden bytes, run writes through the product's own region
// normalisation, and make delays and failures one-shot / resettable so every browser test starts clean.

const goldenDir = "../../../../internal/liff/testdata"

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	e, err := newServer(goldenDir)
	requireNoError(t, err)
	ts := httptest.NewServer(e)
	t.Cleanup(ts.Close)
	return ts
}

func send(t *testing.T, ts *httptest.Server, method, path, body, bearer string) (status int, out []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, ts.URL+path, rd)
	requireNoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ts.Client().Do(req)
	requireNoError(t, err)
	defer closeBody(t, resp)
	out, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	requireNoError(t, err)
	return resp.StatusCode, out
}

func call(t *testing.T, ts *httptest.Server, method, path, body string) (status int, out []byte) {
	t.Helper()
	return send(t, ts, method, "/api/v1/me"+path, body, fakeIDValue)
}

func control(t *testing.T, ts *httptest.Server, path, body string) int {
	t.Helper()
	code, _ := send(t, ts, http.MethodPost, "/__e2e"+path, body, "")
	return code
}

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, name+".json"))
	requireNoError(t, err)
	return b
}

func compact(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	requireNoError(t, json.Compact(&buf, raw))
	return buf.String()
}

func snapshotOf(t *testing.T, ts *httptest.Server) snapshot {
	t.Helper()
	code, body := send(t, ts, http.MethodGet, "/__e2e/state", "", "")
	requireEqual(t, http.StatusOK, code)
	var s snapshot
	requireNoError(t, json.Unmarshal(body, &s))
	return s
}

func TestGoldenFilesServedVerbatim(t *testing.T) {
	ts := newTestServer(t)
	tests := []struct {
		name   string
		method string
		path   string
		golden string
	}{
		{"account read", http.MethodGet, "/account", "account"},
		{"account sync", http.MethodPost, "/account/sync", "account"},
		{"summary", http.MethodGet, "/summary?days=7", "summary"},
		{"day", http.MethodGet, "/day?date=2026-11-02", "day"},
		{"month with the golden label", http.MethodGet, "/month?month=2026-11", "month"},
		{"profile", http.MethodGet, "/profile", "profile"},
		{"alerts as seeded", http.MethodGet, "/alerts", "alerts"},
		{"meal scale", http.MethodPatch, "/meals/m2", "scale"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := call(t, ts, tt.method, tt.path, "")
			requireEqual(t, http.StatusOK, code)
			assertJSONEq(t, string(readGolden(t, tt.golden)), string(body))
			assertEqual(t, compact(t, readGolden(t, tt.golden)), compact(t, body))
		})
	}
}

func TestAPIRejectsBadRequests(t *testing.T) {
	ts := newTestServer(t)
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		bearer string
		want   int
	}{
		{"no bearer", http.MethodGet, "/api/v1/me/summary", "", "", http.StatusUnauthorized},
		{"wrong bearer", http.MethodGet, "/api/v1/me/summary", "", "other-token", http.StatusUnauthorized},
		{"unknown route", http.MethodGet, "/api/v1/me/nope", "", fakeIDValue, http.StatusNotFound},
		{"invalid month label", http.MethodGet, "/api/v1/me/month?month=2026-13", "", fakeIDValue, http.StatusBadRequest},
		{"unknown alert type", http.MethodPut, "/api/v1/me/alerts/other", "", fakeIDValue, http.StatusBadRequest},
		{"weather without regions", http.MethodPut, "/api/v1/me/alerts/weather_warning", "", fakeIDValue, http.StatusBadRequest},
		{"weather with an unknown region", http.MethodPut, "/api/v1/me/alerts/weather_warning", `{"regions":["火星市"]}`, fakeIDValue, http.StatusBadRequest},
		{"weather with a broken body", http.MethodPut, "/api/v1/me/alerts/weather_warning", `{"regions":`, fakeIDValue, http.StatusBadRequest},
		{"control with an unknown field", http.MethodPost, "/__e2e/fail", `{"prefix":"/a","status":500,"extra":1}`, "", http.StatusBadRequest},
		{"control fail with a success status", http.MethodPost, "/__e2e/fail", `{"prefix":"/a","status":200}`, "", http.StatusBadRequest},
		{"control delay without ms", http.MethodPost, "/__e2e/delay", `{"prefix":"/a","ms":0}`, "", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _ := send(t, ts, tt.method, tt.path, tt.body, tt.bearer)
			assertEqual(t, tt.want, code)
		})
	}
}

func TestMonthOtherThanGoldenIsAllZero(t *testing.T) {
	ts := newTestServer(t)
	tests := []struct {
		label string
		days  int
	}{
		{"2026-10", 31},
		{"2026-09", 30},
		{"2028-02", 29},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			code, body := call(t, ts, http.MethodGet, "/month?month="+tt.label, "")
			requireEqual(t, http.StatusOK, code)
			var m monthView
			requireNoError(t, json.Unmarshal(body, &m))
			assertEqual(t, tt.label, m.Month)
			requireLen(t, m.Days, tt.days)
			for _, d := range m.Days {
				assertEqual(t, 0, d.Meals+d.Workouts)
				assertEmpty(t, d.Grade)
			}
		})
	}
}

func TestPutWeatherAlertGoesThroughNormalizeRegions(t *testing.T) {
	ts := newTestServer(t)
	code, body := call(t, ts, http.MethodPut, "/alerts/weather_warning", `{"regions":["臺北市","新北市板橋區","台北市"]}`)
	requireEqual(t, http.StatusOK, code)
	assertEqual(t, compact(t, readGolden(t, "alert_item")), compact(t, body))
	_, listed := call(t, ts, http.MethodGet, "/alerts", "")
	assertEqual(t, compact(t, readGolden(t, "alerts")), compact(t, listed))
}

func TestAlertsFollowWrites(t *testing.T) {
	ts := newTestServer(t)
	code, _ := call(t, ts, http.MethodDelete, "/alerts/earthquake_tsunami", "")
	requireEqual(t, http.StatusNoContent, code)
	_, listed := call(t, ts, http.MethodGet, "/alerts", "")
	assertEqual(t, compact(t, readGolden(t, "alerts_weather_only")), compact(t, listed))

	code, _ = call(t, ts, http.MethodDelete, "/alerts/weather_warning", "")
	requireEqual(t, http.StatusNoContent, code)
	_, listed = call(t, ts, http.MethodGet, "/alerts", "")
	assertJSONEq(t, `{"subscriptions":[]}`, string(listed))

	code, _ = call(t, ts, http.MethodPut, "/alerts/earthquake_tsunami", "")
	requireEqual(t, http.StatusOK, code)
	_, listed = call(t, ts, http.MethodGet, "/alerts", "")
	assertJSONEq(t, `{"subscriptions":[{"alertType":"earthquake_tsunami","regions":[]}]}`, string(listed))
}

func TestFailRuleIsPersistentUntilCleared(t *testing.T) {
	ts := newTestServer(t)
	requireEqual(t, http.StatusNoContent, control(t, ts, "/fail", `{"prefix":"/alerts","method":"GET","status":500}`))
	for i := 0; i < 2; i++ {
		code, body := call(t, ts, http.MethodGet, "/alerts", "")
		assertEqual(t, http.StatusInternalServerError, code)
		assertContains(t, string(body), `"code":"5001"`)
	}
	code, _ := call(t, ts, http.MethodPut, "/alerts/earthquake_tsunami", "")
	assertEqual(t, http.StatusOK, code, "a rule scoped to GET must not touch PUT")
	requireEqual(t, http.StatusNoContent, control(t, ts, "/clear", ""))
	code, _ = call(t, ts, http.MethodGet, "/alerts", "")
	assertEqual(t, http.StatusOK, code)
}

func waitHeld(t *testing.T, ts *httptest.Server, want int) {
	t.Helper()
	requireEventually(t, func() bool { return snapshotOf(t, ts).Held == want }, 3*time.Second, 10*time.Millisecond)
}

func TestDelayHoldsOneRequestThenReleases(t *testing.T) {
	ts := newTestServer(t)
	requireEqual(t, http.StatusNoContent, control(t, ts, "/delay", `{"prefix":"/summary","method":"GET","ms":30000,"late":false}`))
	var wg sync.WaitGroup
	var heldCode int
	wg.Add(1)
	go func() {
		defer wg.Done()
		heldCode, _ = call(t, ts, http.MethodGet, "/summary", "")
	}()
	waitHeld(t, ts, 1)

	start := time.Now()
	code, _ := call(t, ts, http.MethodGet, "/summary", "")
	assertEqual(t, http.StatusOK, code)
	assertLess(t, time.Since(start), 2*time.Second, "the delay is one-shot: the second request must not wait")

	requireEqual(t, http.StatusNoContent, control(t, ts, "/release", ""))
	wg.Wait()
	assertEqual(t, http.StatusOK, heldCode)
	assertEqual(t, 0, snapshotOf(t, ts).Held)
}

func TestDelayRunsOutOnItsOwn(t *testing.T) {
	ts := newTestServer(t)
	requireEqual(t, http.StatusNoContent, control(t, ts, "/delay", `{"prefix":"/day","ms":150}`))
	start := time.Now()
	code, _ := call(t, ts, http.MethodGet, "/day?date=2026-11-02", "")
	assertEqual(t, http.StatusOK, code)
	assertAtLeast(t, time.Since(start), 120*time.Millisecond)
}

func TestLateAndEarlyDelayAroundAccountDeletion(t *testing.T) {
	tests := []struct {
		name string
		rule string
		want int
	}{
		{"early reply is computed before the deletion and stays 200", `{"prefix":"/profile","method":"PUT","ms":30000,"late":false}`, http.StatusOK},
		{"late reply is computed after the deletion and answers 410", `{"prefix":"/profile","method":"PUT","ms":30000,"late":true}`, http.StatusGone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t)
			requireEqual(t, http.StatusNoContent, control(t, ts, "/delay", tt.rule))
			var wg sync.WaitGroup
			var code int
			var body []byte
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, body = call(t, ts, http.MethodPut, "/profile", `{"weightKg":71,"heightCm":175,"age":30,"sex":"male"}`)
			}()
			waitHeld(t, ts, 1)
			gone, _ := call(t, ts, http.MethodDelete, "/account", "")
			requireEqual(t, http.StatusNoContent, gone)
			requireEqual(t, http.StatusNoContent, control(t, ts, "/release", ""))
			wg.Wait()
			assertEqual(t, tt.want, code)
			if tt.want == http.StatusGone {
				assertContains(t, string(body), `"code":"4010"`)
			}
			_, acct := call(t, ts, http.MethodGet, "/account", "")
			assertEqual(t, compact(t, readGolden(t, "account_empty")), compact(t, acct))
		})
	}
}

func TestWriteAfterDeletionIsNotRefused(t *testing.T) {
	ts := newTestServer(t)
	code, _ := call(t, ts, http.MethodDelete, "/account", "")
	requireEqual(t, http.StatusNoContent, code)
	code, _ = call(t, ts, http.MethodPut, "/profile", `{"weightKg":60,"heightCm":170,"age":25,"sex":"female"}`)
	assertEqual(t, http.StatusOK, code, "only writes that were already in flight at deletion time answer 410")
}

func TestResetRestoresGoldenAndClearsRules(t *testing.T) {
	ts := newTestServer(t)
	requireEqual(t, http.StatusNoContent, control(t, ts, "/fail", `{"prefix":"/summary","status":500}`))
	code, _ := call(t, ts, http.MethodDelete, "/account", "")
	requireEqual(t, http.StatusNoContent, code)
	requireEqual(t, http.StatusNoContent, control(t, ts, "/alerts", `{"quake":false,"weather":null}`))
	assertTrue(t, snapshotOf(t, ts).Deleted)

	requireEqual(t, http.StatusNoContent, control(t, ts, "/reset", ""))
	s := snapshotOf(t, ts)
	assertFalse(t, s.Deleted)
	assertTrue(t, s.Quake)
	assertEqual(t, []string{"台北市", "新北市板橋區"}, s.Weather)
	assertEmpty(t, s.Calls)
	code, body := call(t, ts, http.MethodGet, "/summary", "")
	requireEqual(t, http.StatusOK, code)
	assertEqual(t, compact(t, readGolden(t, "summary")), compact(t, body))
}

func TestCallLogRecordsOrderAndStatus(t *testing.T) {
	ts := newTestServer(t)
	call(t, ts, http.MethodPost, "/account/sync", "")
	call(t, ts, http.MethodGet, "/nope", "")
	s := snapshotOf(t, ts)
	requireLen(t, s.Calls, 2)
	assertEqual(t, callRecord{Seq: 1, Method: http.MethodPost, Path: "/account/sync", Status: http.StatusOK}, s.Calls[0])
	assertEqual(t, http.StatusNotFound, s.Calls[1].Status)
	assertEqual(t, 2, s.Calls[1].Seq)
}

func TestPageIsTheRealOneWithItsSecurityHeaders(t *testing.T) {
	ts := newTestServer(t)
	tests := []struct {
		path string
		want string
	}{
		{"/liff", `data-liff-id="2001234567-AbCdEfGh"`},
		{"/liff/", `data-liff-id="2001234567-AbCdEfGh"`},
		{"/liff/static/app.js", "use strict"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+tt.path, http.NoBody)
			requireNoError(t, err)
			resp, err := ts.Client().Do(req)
			requireNoError(t, err)
			defer closeBody(t, resp)
			body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			requireNoError(t, err)
			assertEqual(t, http.StatusOK, resp.StatusCode)
			assertContains(t, string(body), tt.want)
			assertContains(t, resp.Header.Get("Content-Security-Policy"), "script-src 'self' https://static.line-scdn.net")
			assertEqual(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
		})
	}
}

func TestRunRefusesUnsafeArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"non loopback address", []string{"-addr", "0.0.0.0:0", "-golden-dir", goldenDir}, "loopback"},
		{"hostname instead of an IP", []string{"-addr", "example.com:0", "-golden-dir", goldenDir}, "loopback"},
		{"missing golden dir flag", []string{"-addr", "127.0.0.1:0"}, "-golden-dir is required"},
		{"golden dir without the fixtures", []string{"-addr", "127.0.0.1:0", "-golden-dir", t.TempDir()}, "read golden"},
		{"address without a port", []string{"-addr", "127.0.0.1", "-golden-dir", goldenDir}, "bad -addr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(tt.args, &out)
			requireError(t, err)
			assertContains(t, err.Error(), tt.want)
			assertEmpty(t, out.String(), "nothing may be announced when startup is refused")
		})
	}
}

func closeBody(t *testing.T, resp *http.Response) {
	t.Helper()
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close response body: %v", err)
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func requireError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func requireEqual(t *testing.T, want, got any) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("want %#v, got %#v", want, got)
	}
}

func assertEqual(t *testing.T, want, got any, msg ...string) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Errorf("%swant %#v, got %#v", prefix(msg), want, got)
	}
}

func prefix(msg []string) string {
	if len(msg) == 0 {
		return ""
	}
	return msg[0] + ": "
}

func requireLen(t *testing.T, v any, want int) {
	t.Helper()
	if got := reflect.ValueOf(v).Len(); got != want {
		t.Fatalf("want length %d, got %d", want, got)
	}
}

func assertEmpty(t *testing.T, v any, msg ...string) {
	t.Helper()
	if reflect.ValueOf(v).Len() != 0 {
		t.Errorf("%sexpected empty, got %#v", prefix(msg), v)
	}
}

func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%q does not contain %q", haystack, needle)
	}
}

func assertJSONEq(t *testing.T, want, got string) {
	t.Helper()
	var w, g any
	requireNoError(t, json.Unmarshal([]byte(want), &w))
	requireNoError(t, json.Unmarshal([]byte(got), &g))
	if !reflect.DeepEqual(w, g) {
		t.Errorf("JSON differs:\nwant %s\n got %s", want, got)
	}
}

func assertTrue(t *testing.T, v bool) {
	t.Helper()
	if !v {
		t.Error("expected true")
	}
}

func assertFalse(t *testing.T, v bool) {
	t.Helper()
	if v {
		t.Error("expected false")
	}
}

func assertLess(t *testing.T, got, limit time.Duration, msg string) {
	t.Helper()
	if got >= limit {
		t.Errorf("%s: %v is not below %v", msg, got, limit)
	}
}

func assertAtLeast(t *testing.T, got, floor time.Duration) {
	t.Helper()
	if got < floor {
		t.Errorf("%v is below %v", got, floor)
	}
}

func requireEventually(t *testing.T, cond func() bool, wait, tick time.Duration) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", wait)
		}
		time.Sleep(tick)
	}
}
