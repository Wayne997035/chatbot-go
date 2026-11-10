package liff

import (
	"bytes"
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	"chatbot-go/internal/storage/storagetest"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// erasureUID LINE userID 的真實格式（U 加 32 位小寫 hex）；注入的錯誤字串都內含它，log 若沒遮蔽就會外洩.
const erasureUID = "U0123456789abcdef0123456789abcdef"

const validProfileBody = `{"weightKg":68,"heightCm":170,"age":30,"sex":"female"}`

// erasureProbe 假的 ErasureChecker：記錄收到的 userID、since 與 ctx 狀態；照 go-redis 的行為，ctx 已取消就查不到（回 false）.
type erasureProbe struct {
	mu sync.Mutex
	// erased 固定回答；markedAt 非零時改成「標記時間不早於 since 即算已刪除」.
	erased   bool
	markedAt time.Time

	calls       int
	userID      string
	since       time.Time
	ctxErr      error
	hasDeadline bool
	remaining   time.Duration
}

func (p *erasureProbe) ErasedSince(ctx context.Context, userID string, since time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.userID, p.since, p.ctxErr = userID, since, ctx.Err()
	if dl, ok := ctx.Deadline(); ok {
		p.hasDeadline, p.remaining = true, time.Until(dl)
	}
	if ctx.Err() != nil {
		return false
	}
	if !p.markedAt.IsZero() {
		return !p.markedAt.Before(since)
	}
	return p.erased
}

// erasureProfiles 包住 storagetest.Profiles：Upsert 先存值，再執行 afterUpsert、回 upsertErr；Delete 可回錯並記錄收到的 ctx.
type erasureProfiles struct {
	*storagetest.Profiles
	afterUpsert func()
	upsertErr   error
	deleteErr   error

	deleteCalls  int
	deleteCtxErr error
}

func (p *erasureProfiles) Upsert(ctx context.Context, pr *profile.Profile) error {
	if err := p.Profiles.Upsert(ctx, pr); err != nil {
		return err
	}
	if p.afterUpsert != nil {
		p.afterUpsert()
	}
	return p.upsertErr
}

func (p *erasureProfiles) Delete(ctx context.Context, userID string) error {
	p.deleteCalls++
	p.deleteCtxErr = ctx.Err()
	if p.deleteErr != nil {
		return p.deleteErr
	}
	return p.Profiles.Delete(ctx, userID)
}

// newProfileAPI 建一個只掛 /me/profile 的 API；checker 為 nil 表示不呼叫 WithErasureCheck.
func newProfileAPI(t *testing.T, ps ProfileStore, checker ErasureChecker) *echo.Echo {
	t.Helper()
	v, _ := newVerifier(t)
	e := echo.New()
	h := NewHandler(&storagetest.Meals{}, &storagetest.Workouts{}, ps)
	if checker != nil {
		h.WithErasureCheck(checker)
	}
	h.now = func() time.Time { return fixedNow }
	h.Routes(e.Group("/api/v1/me", NoStore, Auth(v)))
	return e
}

// putProfile 用指定 ctx 送 PUT /me/profile.
func putProfile(t *testing.T, ctx context.Context, e *echo.Echo) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodPut, "/api/v1/me/profile", strings.NewReader(validProfileBody))
	req.Header.Set("Authorization", "Bearer good-"+erasureUID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func newErasureProfiles() *erasureProfiles {
	return &erasureProfiles{Profiles: storagetest.NewProfiles()}
}

func (p *erasureProfiles) has() bool {
	_, ok := p.Items[erasureUID]
	return ok
}

func statusCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp httputil.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Status == nil {
		t.Fatalf("response %q has no status: %v", rec.Body.String(), err)
	}
	return resp.Status.Code
}

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestPutProfileErasedAfterWriteReturns410 [F291-08] 儲存之後才發現使用者已刪除資料：補刪、回 410（錯誤碼 4010）.
func TestPutProfileErasedAfterWriteReturns410(t *testing.T) {
	tests := []struct {
		name        string
		erased      bool
		wantStatus  int
		wantCode    string
		wantProfile bool
		wantDeletes int
	}{
		{"erased after write is undone with 410", true, http.StatusGone, "4010", false, 1},
		{"not erased keeps the profile with 200", false, http.StatusOK, "", true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps := newErasureProfiles()
			probe := &erasureProbe{erased: tt.erased}

			rec := putProfile(t, context.Background(), newProfileAPI(t, ps, probe))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantCode != "" && statusCode(t, rec) != tt.wantCode {
				t.Errorf("code = %s, want %s", statusCode(t, rec), tt.wantCode)
			}
			if ps.has() != tt.wantProfile || ps.deleteCalls != tt.wantDeletes {
				t.Errorf("profile present = %v deletes = %d, want %v and %d", ps.has(), ps.deleteCalls, tt.wantProfile, tt.wantDeletes)
			}
			if probe.userID != erasureUID || !probe.since.Equal(fixedNow) {
				t.Errorf("checker got user %q since %v, want %q and %v", probe.userID, probe.since, erasureUID, fixedNow)
			}
		})
	}
}

// TestPutProfileUndoFailureReturns500 [F291-08] 補刪失敗：回 500（不能宣稱已刪除），log 遮蔽 userID.
func TestPutProfileUndoFailureReturns500(t *testing.T) {
	buf := captureSlog(t)
	ps := newErasureProfiles()
	ps.deleteErr = errors.New("delete profile failed for " + erasureUID)

	rec := putProfile(t, context.Background(), newProfileAPI(t, ps, &erasureProbe{erased: true}))

	if rec.Code != http.StatusInternalServerError || statusCode(t, rec) != "5001" {
		t.Fatalf("status = %d code = %s, want 500 and 5001: %s", rec.Code, statusCode(t, rec), rec.Body.String())
	}
	logs := buf.String()
	if strings.Contains(logs, erasureUID) || !strings.Contains(logs, "U***") {
		t.Errorf("log must mask the userID as U***: %s", logs)
	}
}

// TestPutProfileUpsertErrorThenErasedReturns410 [F291-08] Upsert 回錯也要複查（可能已落地）：命中回 410；沒命中維持 500.
func TestPutProfileUpsertErrorThenErasedReturns410(t *testing.T) {
	tests := []struct {
		name       string
		erased     bool
		wantStatus int
		wantCode   string
	}{
		{"upsert error and erased is 410", true, http.StatusGone, "4010"},
		{"upsert error and not erased stays 500", false, http.StatusInternalServerError, "5001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps := newErasureProfiles()
			ps.upsertErr = errors.New("write failed after landing")

			rec := putProfile(t, context.Background(), newProfileAPI(t, ps, &erasureProbe{erased: tt.erased}))

			if rec.Code != tt.wantStatus || statusCode(t, rec) != tt.wantCode {
				t.Errorf("status = %d code = %s, want %d and %s", rec.Code, statusCode(t, rec), tt.wantStatus, tt.wantCode)
			}
			if tt.erased && ps.has() {
				t.Error("a write that errored after landing must still be removed")
			}
		})
	}
}

// TestPutProfileAfterErasureIsAccepted [F291-08][SEC29-02][F292-02] 刪除發生在請求到達之前：照常 200；同一個 fake 標記不早於 since（含同一毫秒）就 410（證明 since 比較真的在作用）.
func TestPutProfileAfterErasureIsAccepted(t *testing.T) {
	tests := []struct {
		name       string
		markedAt   time.Time
		wantStatus int
		wantStored bool
	}{
		{"marker before the request is accepted", fixedNow.Add(-time.Minute), http.StatusOK, true},
		{"marker after the request is rejected", fixedNow.Add(time.Minute), http.StatusGone, false},
		{"marker in the same millisecond is rejected", fixedNow, http.StatusGone, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps := newErasureProfiles()

			rec := putProfile(t, context.Background(), newProfileAPI(t, ps, &erasureProbe{markedAt: tt.markedAt}))

			if rec.Code != tt.wantStatus || ps.has() != tt.wantStored {
				t.Errorf("status = %d stored = %v, want %d and %v", rec.Code, ps.has(), tt.wantStatus, tt.wantStored)
			}
		})
	}
}

// TestPutProfileRecheckSurvivesCancelledContext [F291-08] 請求 ctx 在複查前被取消：複查與補刪仍照做（沒取消的 ctx、最多 5 秒）.
func TestPutProfileRecheckSurvivesCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ps := newErasureProfiles()
	ps.afterUpsert = cancel // 寫入完成、複查之前請求被取消
	probe := &erasureProbe{erased: true}

	rec := putProfile(t, ctx, newProfileAPI(t, ps, probe))

	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410: %s", rec.Code, rec.Body.String())
	}
	if probe.ctxErr != nil || ps.deleteCtxErr != nil {
		t.Errorf("checker ctx err = %v, delete ctx err = %v, want both nil", probe.ctxErr, ps.deleteCtxErr)
	}
	if !probe.hasDeadline || probe.remaining <= 0 || probe.remaining > 5*time.Second {
		t.Errorf("deadline present = %v remaining = %v, want in (0, 5s]", probe.hasDeadline, probe.remaining)
	}
	if ps.has() {
		t.Error("profile must be removed")
	}
}

// TestPutProfileWithoutErasureCheckIsUnchanged [F291-08] 沒呼叫 WithErasureCheck：行為同現況；
// 同一個請求若注入「已刪除」的 fake 則必須 410（證明「未注入 = 不檢查」不是恆不檢查）.
func TestPutProfileWithoutErasureCheckIsUnchanged(t *testing.T) {
	t.Run("not injected", func(t *testing.T) {
		ps := newErasureProfiles()

		rec := putProfile(t, context.Background(), newProfileAPI(t, ps, nil))

		if rec.Code != http.StatusOK || !ps.has() || ps.deleteCalls != 0 {
			t.Errorf("status = %d stored = %v deletes = %d, want 200 true 0", rec.Code, ps.has(), ps.deleteCalls)
		}
	})
	t.Run("injected and erased", func(t *testing.T) {
		ps := newErasureProfiles()

		rec := putProfile(t, context.Background(), newProfileAPI(t, ps, &erasureProbe{erased: true}))

		if rec.Code != http.StatusGone || ps.has() {
			t.Errorf("status = %d stored = %v, want 410 false", rec.Code, ps.has())
		}
	})
}

// TestFrontendProfileSaveHandles410 [F291-09][F292-06] 字串冒煙：app.js 儲存失敗時依狀態碼顯示對的文案，410 之後重新載入；
// 這只確認文案與分支沒有被刪，不代表畫面正確，行為證據見 PR #29 修正輪的真瀏覽器紀錄.
func TestFrontendProfileSaveHandles410(t *testing.T) {
	js, err := webFS.ReadFile("web/static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, want := range []string{
		`"http-410"`, `"http-400"`, "你的資料已經刪除，這筆沒有儲存", "儲存失敗，請確認數值是否合理", "儲存失敗，請稍後再試",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("app.js is missing %q", want)
		}
	}
	// 410 分支到下一個 return; 之間必須重新載入，且讀取失敗另外提示（loadAll().catch(），畫面才會跟著清空.
	_, branch, found := strings.Cut(src, `"http-410"`)
	if found {
		branch, _, found = strings.Cut(branch, "return;")
	}
	if !found || !strings.Contains(branch, "loadAll().catch(") {
		t.Errorf("the http-410 branch must reload with loadAll().catch( before returning: %q", branch)
	}
	if strings.Contains(src, "innerHTML") {
		t.Error("app.js must keep building DOM with textContent only")
	}
}

// TestFrontendReloadFailureIsNotActionFailure [F293-01] 字串冒煙：操作（儲存、刪除、改份量）成功後的重新載入自己處理讀取失敗，
// 不落進操作失敗的分支（否則已經做成卻顯示「失敗」）. 畫面行為沒有自動化覆蓋.
func TestFrontendReloadFailureIsNotActionFailure(t *testing.T) {
	js, err := webFS.ReadFile("web/static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(js), ".then(loadAll)") {
		t.Error("app.js chains .then(loadAll) after an action: a reload failure would reach the action's error branch")
	}
	if n := strings.Count(string(js), "return loadAll()"); n != 1 {
		t.Errorf("app.js has %d `return loadAll()`; only the initial page load may return it (a reload failure chained into an action's promise reaches its error branch)", n)
	}
	if n := strings.Count(string(js), "loadAll().catch("); n != 5 {
		t.Errorf("app.js has %d `loadAll().catch(`, want 5 (two existing handlers plus delete, scale, save): every reload after an action must handle its own failure", n)
	}
	if n := strings.Count(string(js), "loadAll("); n != 7 {
		t.Errorf("app.js has %d `loadAll(` occurrences, want 7 (definition, initial page load, five .catch reloads): an extra reload call may chain a failure into an action's error branch", n)
	}
	if n := strings.Count(string(js), `loadAll().catch(function () { showStatus("讀取失敗，請稍後再試", true); });`); n != 4 {
		t.Errorf("app.js has %d reloads that only say 讀取失敗, want 4 (409, 410, delete, save): a reload failure must not be reported as the action failing", n)
	}
	if !strings.Contains(string(js), `loadAll().catch(function () { select.disabled = false; showStatus("讀取失敗，請稍後再試", true); });`) {
		t.Error("after a successful scale change whose reload fails, the select must be re-enabled")
	}
	_, success, found := strings.Cut(string(js), `showStatus("已儲存");`)
	if found {
		success, _, found = strings.Cut(success, "function (err)")
	}
	if !found || !strings.Contains(success, "loadAll().catch(") || strings.Contains(success, "return loadAll()") {
		t.Errorf("after a successful save, loadAll() must handle its own failure instead of reaching the save error branch: %q", success)
	}
}

// TestScaleMealAfterErasureReturns404[F291-11] 資料被刪後改份量：404，不把餐點建回來（只鎖現況，產品碼不動）.
func TestScaleMealAfterErasureReturns404(t *testing.T) {
	t.Run("erased meal is not recreated", func(t *testing.T) {
		f := newAPI(t) // 刪除後：沒有任何餐點

		rec := f.do(http.MethodPatch, "/api/v1/me/meals/m1", "good-U1", `{"scale":2}`)

		if rec.Code != http.StatusNotFound || statusCode(t, rec) != "4001" {
			t.Errorf("status = %d code = %s, want 404 and 4001: %s", rec.Code, statusCode(t, rec), rec.Body.String())
		}
		if len(f.meals.Items) != 0 {
			t.Errorf("meals = %+v, want none recreated", f.meals.Items)
		}
	})
	t.Run("existing meal is scaled", func(t *testing.T) {
		f := newAPI(t)
		items := []meal.FoodItem{{Name: "牛肉麵", Nutrients: meal.Nutrients{Calories: 650}}}
		f.meals.Items = []meal.Meal{{ID: "m1", UserID: "U1", EatenAt: fixedNow, Items: items, Total: items[0].Nutrients}}

		rec := f.do(http.MethodPatch, "/api/v1/me/meals/m1", "good-U1", `{"scale":2}`)

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})
}
