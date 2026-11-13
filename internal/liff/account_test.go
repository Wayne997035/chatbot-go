package liff

import (
	"bytes"
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/storage/storagetest"
	"chatbot-go/internal/subscription"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	userstore "chatbot-go/internal/storage/database/user"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

// 本檔的 package 層級名字一律 acct 前綴（D6b：與畫面組的 liff_test.go / page_test.go 同 package，不得重名）.

// acctSecretName / acctSecretEmail 會員資料的測試哨兵：出現在任何 log 就紅.
const (
	acctSecretName  = "SECRET-NAME"
	acctSecretEmail = "secret@x.test"
	acctUID         = "U0123456789abcdef0123456789abcdef"
)

// acctLeakyErr 內含 userID、email、主機:埠的儲存錯誤；log 若沒經 redact 就會外洩.
func acctLeakyErr(what string) error {
	return errors.New(what + " failed for " + acctUID + " a@b.test at 10.0.0.5:27017")
}

// acctFakeLINE 模擬 LINE 驗證端點：
// good-<id> 回 name / picture / email；bare-<id> 只回 iss/sub/aud/exp（沒要求 profile / email scope）；
// secret-<id> 回哨兵名稱與 email；short-<id> 同 good 但 5 分鐘後到期；expired / other-aud / down 同既有案例.
func acctFakeLINE(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		token, aud := form.Get("id_token"), form.Get("client_id")
		exp := time.Now().Add(time.Hour).Unix()
		claims := map[string]any{"iss": lineIssuer, "aud": aud}
		prefix, id, _ := strings.Cut(token, "-")
		switch {
		case token == "down":
			w.WriteHeader(http.StatusBadGateway)
			return
		case token == "expired":
			exp = time.Now().Add(-time.Minute).Unix()
		case token == "other-aud":
			claims["aud"] = "999"
		case prefix == "good" || prefix == "short":
			claims["sub"] = id
			claims["name"] = "Name-" + id
			claims["picture"] = "https://example.test/" + id + ".png"
			claims["email"] = strings.ToLower(id) + "@example.test"
			if prefix == "short" {
				exp = time.Now().Add(5 * time.Minute).Unix()
			}
		case prefix == "bare":
			claims["sub"] = id
		case prefix == "secret":
			claims["sub"] = id
			claims["name"] = acctSecretName
			claims["picture"] = "https://example.test/secret.png"
			claims["email"] = acctSecretEmail
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, ok := claims["sub"]; !ok {
			claims["sub"] = strings.TrimPrefix(token, "other-aud")
		}
		claims["exp"] = exp
		_ = json.NewEncoder(w).Encode(claims)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// acctUsers 假的 AccountStore：可注入錯誤、記錄呼叫與 ctx 狀態.
type acctUsers struct {
	mu           sync.Mutex
	docs         map[string]userstore.User
	saves        int
	deletes      int
	saveErr      error
	deleteErr    error
	findErr      error
	afterSave    func()
	deleteCtxErr error
}

func (u *acctUsers) FindByID(_ context.Context, id string) (*userstore.User, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.findErr != nil {
		return nil, u.findErr
	}
	d, ok := u.docs[id]
	if !ok {
		return nil, nil
	}
	return &d, nil
}

func (u *acctUsers) SaveLineProfile(_ context.Context, userID, name, pictureURL, email string) error {
	u.mu.Lock()
	u.saves++
	if u.saveErr == nil {
		d := u.docs[userID]
		d.ID, d.DisplayName, d.PictureURL, d.Email = userID, name, pictureURL, email
		if d.CreateTime.IsZero() {
			d.CreateTime = time.Now()
		}
		u.docs[userID] = d
	}
	hook, err := u.afterSave, u.saveErr
	u.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (u *acctUsers) Delete(ctx context.Context, id string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.deletes++
	u.deleteCtxErr = ctx.Err()
	if u.deleteErr != nil {
		return u.deleteErr
	}
	delete(u.docs, id)
	return nil
}

// acctEraser 假的 AccountEraser：記錄呼叫、每一步前檢查 ctx、可阻塞到 ctx 結束.
type acctEraser struct {
	mu       sync.Mutex
	calls    int
	userID   string
	err      error
	steps    int
	stepsRun int
	ctxErrs  []error
	// beforeSteps 在第一步前呼叫（例如取消請求 ctx）.
	beforeSteps func()
	// block 為 true 時等到 ctx 結束才回（模擬卡住的刪除）.
	block bool
}

func (e *acctEraser) EraseAll(ctx context.Context, userID string) error {
	e.mu.Lock()
	e.calls++
	e.userID = userID
	e.mu.Unlock()
	if e.beforeSteps != nil {
		e.beforeSteps()
	}
	if e.block {
		<-ctx.Done()
		return ctx.Err()
	}
	for i := 0; i < e.steps; i++ {
		e.mu.Lock()
		e.ctxErrs = append(e.ctxErrs, ctx.Err())
		if ctx.Err() == nil {
			e.stepsRun++
		}
		e.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	return e.err
}

// acctSubStore 假的訂閱 store（Upsert / Delete / FindByUserID）.
type acctSubStore struct {
	mu           sync.Mutex
	docs         map[string]alertsub.AlertSubscription
	upserts      int
	deletes      int
	upsertErr    error
	deleteErr    error
	listErr      error
	afterUpsert  func()
	deleteCtxErr error
}

func (s *acctSubStore) Upsert(_ context.Context, sub *alertsub.AlertSubscription) error {
	s.mu.Lock()
	s.upserts++
	if s.upsertErr == nil {
		s.docs[sub.ID] = *sub
	}
	hook, err := s.afterUpsert, s.upsertErr
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (s *acctSubStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	s.deleteCtxErr = ctx.Err()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.docs, id)
	return nil
}

func (s *acctSubStore) FindByUserID(_ context.Context, userID string) ([]alertsub.AlertSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []alertsub.AlertSubscription
	for i := range s.docs {
		if s.docs[i].UserID == userID {
			out = append(out, s.docs[i])
		}
	}
	return out, nil
}

// acctFixture 帶齊新依賴的 API：真的 Verifier（接假 LINE）、假 user store / eraser / 訂閱 store、真的訂閱服務.
type acctFixture struct {
	e        *echo.Echo
	h        *Handler
	meals    *storagetest.Meals
	workouts *storagetest.Workouts
	profiles *storagetest.Profiles
	users    *acctUsers
	eraser   *acctEraser
	subStore *acctSubStore
	svc      *subscription.DefaultService
	probe    *erasureProbe
	verifier *Verifier
	line     *httptest.Server
}

func acctNewFixture(t *testing.T) *acctFixture {
	t.Helper()
	srv := acctFakeLINE(t)
	v := NewVerifier(channelID, srv.Client(), nil)
	v.verifyURL = srv.URL
	f := &acctFixture{
		e: echo.New(), meals: &storagetest.Meals{}, workouts: &storagetest.Workouts{}, profiles: storagetest.NewProfiles(),
		users:    &acctUsers{docs: map[string]userstore.User{}},
		eraser:   &acctEraser{steps: 3},
		subStore: &acctSubStore{docs: map[string]alertsub.AlertSubscription{}},
		probe:    &erasureProbe{},
		verifier: v, line: srv,
	}
	f.svc = subscription.New(f.subStore).WithErasureCheck(f.probe)
	f.h = NewHandler(f.meals, f.workouts, f.profiles).
		WithErasureCheck(f.probe).WithClaimsVerifier(v).WithAccountStore(f.users).WithAccountEraser(f.eraser).WithSubscription(f.svc)
	f.h.now = func() time.Time { return fixedNow }
	f.h.Routes(f.e.Group("/api/v1/me", NoStore, Auth(v)))
	return f
}

func (f *acctFixture) acctDo(method, path, token, body, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	f.e.ServeHTTP(rec, req)
	return rec
}

// do 帶 body 時一律 application/json.
func (f *acctFixture) do(method, path, token, body string) *httptest.ResponseRecorder {
	ct := ""
	if body != "" {
		ct = "application/json"
	}
	return f.acctDo(method, path, token, body, ct)
}

// acctErrCode 取出錯誤回應的 status.code.
func acctErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var r httputil.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil || r.Status == nil {
		t.Fatalf("not an error response: %q (%v)", rec.Body.String(), err)
	}
	return r.Status.Code
}

// acctGolden 逐字比對 testdata（去頭尾空白）.
func acctGolden(t *testing.T, rec *httptest.ResponseRecorder, name string) {
	t.Helper()
	want, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || !bytes.Equal(bytes.TrimSpace(rec.Body.Bytes()), bytes.TrimSpace(want)) {
		t.Errorf("%s: status %d\n got: %s\nwant: %s", name, rec.Code, rec.Body.String(), want)
	}
}

// acctCaptureLog 把預設 slog 換成寫進 buffer 的 handler，結束時還原.
func acctCaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

const acctSyncPath = "/api/v1/me/account/sync"

// ---- F30-03：R1 同步 / R2 查看 ----

// A1：存的是 claims 的值，不是前端 body 夾帶的字串；回應逐字等於 testdata/account.json.
func TestAcctSyncStoresVerifiedClaimsNotRequestBody(t *testing.T) {
	f := acctNewFixture(t)
	rec := f.do(http.MethodPost, acctSyncPath, "good-U1", `{"displayName":"Evil","email":"evil@x.test","pictureUrl":"https://evil.test/x.png"}`)
	acctGolden(t, rec, "account.json")
	d := f.users.docs["U1"]
	if d.DisplayName != "Name-U1" || d.PictureURL != "https://example.test/U1.png" || d.Email != "u1@example.test" {
		t.Errorf("stored %+v, want the claims values", d)
	}
	if d.CreateTime.IsZero() {
		t.Error("createTime must exist")
	}
	if got := f.do(http.MethodGet, "/api/v1/me/account", "good-U1", ""); got.Code != http.StatusOK || got.Body.String() != rec.Body.String() {
		t.Errorf("GET /account after sync = %d %s", got.Code, got.Body.String())
	}
}

// A2：沒有三欄的 token 存空值；先 good 再 bare，舊 email 被清掉（覆寫語意，不殘留）.
func TestAcctSyncBareTokenOverwritesWithEmpty(t *testing.T) {
	f := acctNewFixture(t)
	rec := f.do(http.MethodPost, acctSyncPath, "bare-U1", "")
	acctGolden(t, rec, "account_empty.json")
	if f.do(http.MethodPost, acctSyncPath, "good-U1", "").Code != http.StatusOK {
		t.Fatal("good sync failed")
	}
	if f.users.docs["U1"].Email == "" {
		t.Fatal("setup: good sync must store the email")
	}
	if f.do(http.MethodPost, acctSyncPath, "bare-U1", "").Code != http.StatusOK {
		t.Fatal("bare sync failed")
	}
	if d := f.users.docs["U1"]; d.Email != "" || d.DisplayName != "" || d.PictureURL != "" {
		t.Errorf("stale values survived the bare sync: %+v", d)
	}
}

// A3：清洗規則（控制字元先檢查、再 trim、再長度；name 用 rune、picture / email 用 byte）.
func TestAcctCleanFields(t *testing.T) {
	https1024 := "https://a.test/" + strings.Repeat("x", 1024-len("https://a.test/"))
	email254 := strings.Repeat("a", 254-len("@b.co")) + "@b.co"
	name100CJK := strings.Repeat("字", 100)
	cases := []struct {
		field, name, in, want string
	}{
		{"picture", "http rejected", "http://x/a.png", ""},
		{"picture", "javascript rejected", "javascript:alert(1)", ""},
		{"picture", "1025 bytes rejected", https1024 + "y", ""},
		{"picture", "exactly 1024 bytes kept", https1024, https1024},
		{"picture", "valid kept", "https://profile.line-scdn.net/abc", "https://profile.line-scdn.net/abc"},
		{"picture", "no host rejected", "https:///a.png", ""},
		{"picture", "control char rejected", "https://a.test/\n", ""},
		{"email", "double at rejected", "a@@b", ""},
		{"email", "no at rejected", "no-at", ""},
		{"email", "empty local rejected", "@b.co", ""},
		{"email", "empty domain rejected", "a@", ""},
		{"email", "255 bytes rejected", "a" + email254, ""},
		{"email", "exactly 254 bytes kept", email254, email254},
		{"email", "valid kept", "a@b.co", "a@b.co"},
		{"name", "newline rejected whole", "Ta\nro", ""},
		{"name", "101 runes truncated to 100", name100CJK + "字", name100CJK},
		{"name", "exactly 100 CJK runes (300 bytes) kept", name100CJK, name100CJK},
		{"name", "surrounding spaces trimmed", "  Taro  ", "Taro"},
		{"name", "zero width space is not a control char", "\u200b", "\u200b"},
		{"name", "tab only is control", "\t", ""},
		{"name", "control checked before trim", "\tTaro", ""},
	}
	for _, tc := range cases {
		t.Run(tc.field+"/"+tc.name, func(t *testing.T) {
			var got string
			switch tc.field {
			case "name":
				got = cleanName(tc.in)
			case "picture":
				got = cleanPictureURL(tc.in)
			default:
				got = cleanEmail(tc.in)
			}
			if got != tc.want {
				t.Errorf("clean(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A4：沒有 / 壞 / 過期 / 別 channel 的 token → 401；token 有效但 claims 的 sub 與 context 不同 → 401；都不寫入.
type acctWrongSubVerifier struct{}

func (acctWrongSubVerifier) VerifyClaims(context.Context, string) (*Claims, error) {
	return &Claims{Sub: "U2", Name: "Other"}, nil
}

func TestAcctSyncRejectsBadTokensAndSubMismatch(t *testing.T) {
	f := acctNewFixture(t)
	for _, token := range []string{"", "forged", "expired", "other-aud"} {
		rec := f.do(http.MethodPost, acctSyncPath, token, "")
		if rec.Code != http.StatusUnauthorized || acctErrCode(t, rec) != "1001" {
			t.Errorf("token %q: %d %s", token, rec.Code, rec.Body.String())
		}
	}
	f.h.WithClaimsVerifier(acctWrongSubVerifier{})
	rec := f.do(http.MethodPost, acctSyncPath, "good-U1", "")
	if rec.Code != http.StatusUnauthorized || acctErrCode(t, rec) != "1001" {
		t.Errorf("sub mismatch: %d %s", rec.Code, rec.Body.String())
	}
	if f.users.saves != 0 || len(f.users.docs) != 0 {
		t.Errorf("nothing may be written (saves=%d docs=%v)", f.users.saves, f.users.docs)
	}
}

// A32：VerifyClaims 失敗分類 ① 無效 / 過期 → 401 ② LINE 暫時不可用 → 503（與中介層同 body）③ 中介層快取命中、重新驗證時已過期 → 401.
// acctErrVerifier 中介層通過後，VerifyClaims 回指定錯誤.
type acctErrVerifier struct{ err error }

func (v acctErrVerifier) VerifyClaims(context.Context, string) (*Claims, error) { return nil, v.err }

func TestAcctSyncVerifyClaimsFailureClassification(t *testing.T) {
	f := acctNewFixture(t)
	mw := f.do(http.MethodGet, "/api/v1/me/account", "down", "") // 中介層自己遇到 LINE 掛掉的 503 body
	if mw.Code != http.StatusServiceUnavailable {
		t.Fatalf("setup: middleware outage = %d", mw.Code)
	}
	// ② LINE 暫時不可用（中介層以快取 / 有效 token 通過，VerifyClaims 才遇到）→ 503，body 與中介層相同，不得是 401.
	f.h.WithClaimsVerifier(acctErrVerifier{err: errors.New("LINE verify returned 502")})
	rec := f.do(http.MethodPost, acctSyncPath, "good-U1", "")
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != mw.Body.String() {
		t.Errorf("outage in VerifyClaims: %d %s, middleware body %s", rec.Code, rec.Body.String(), mw.Body.String())
	}
	// ① 無效 / 過期 → 401，不得是 503.
	f.h.WithClaimsVerifier(acctErrVerifier{err: ErrInvalidToken})
	if rec := f.do(http.MethodPost, acctSyncPath, "good-U1", ""); rec.Code != http.StatusUnauthorized || acctErrCode(t, rec) != "1001" {
		t.Errorf("invalid token in VerifyClaims: %d %s", rec.Code, rec.Body.String())
	}
	f.h.WithClaimsVerifier(f.verifier)
	if rec := f.do(http.MethodPost, acctSyncPath, "expired", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired: %d", rec.Code)
	}
	if f.users.saves != 0 {
		t.Errorf("no write on any verification failure, saves=%d", f.users.saves)
	}

	// ③ 兩次驗證之間 token 到期：中介層用快取通過，VerifyClaims 的檢查落在到期之後.
	clock := &testClock{t: time.Now()}
	late := NewVerifier(channelID, f.line.Client(), nil)
	late.verifyURL = f.line.URL
	late.now = clock.Now
	clock.Advance(6 * time.Minute) // short-<id> 5 分鐘到期
	f.h.WithClaimsVerifier(late)
	if rec := f.do(http.MethodGet, "/api/v1/me/account", "short-U1", ""); rec.Code != http.StatusOK {
		t.Fatalf("middleware must accept the token while it is valid: %d", rec.Code)
	}
	rec = f.do(http.MethodPost, acctSyncPath, "short-U1", "")
	if rec.Code != http.StatusUnauthorized || acctErrCode(t, rec) != "1001" || f.users.saves != 0 {
		t.Errorf("token expired between the two verifications: %d %s saves=%d", rec.Code, rec.Body.String(), f.users.saves)
	}
}

// A6：sync 之後 Redis 驗證快取的 value 仍只有 userID，不出現名稱 / email.
func TestAcctSyncNeverCachesProfileInRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	f := acctNewFixture(t)
	v := NewVerifier(channelID, f.line.Client(), rc)
	v.verifyURL = f.line.URL
	f.e = echo.New()
	f.h.WithClaimsVerifier(v).Routes(f.e.Group("/api/v1/me", NoStore, Auth(v)))

	if rec := f.do(http.MethodPost, acctSyncPath, "secret-U1", ""); rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}
	keys := mr.Keys()
	if len(keys) == 0 {
		t.Fatal("expected the middleware to cache the verified userID")
	}
	for _, k := range keys {
		val, err := mr.Get(k)
		if err != nil {
			t.Fatal(err)
		}
		if val != "U1" || strings.Contains(k+val, acctSecretName) || strings.Contains(k+val, acctSecretEmail) {
			t.Errorf("cache %q = %q, want only the userID", k, val)
		}
	}
}

// A7 / A8 / A12：R1 刪除標記防寫回（零值 since、寫前寫後各查一次、補刪不隨請求取消）.
func TestAcctSyncErasureGuards(t *testing.T) {
	cases := []struct {
		name        string
		erased      func(f *acctFixture) // 設定標記行為
		saveErr     error
		deleteErr   error
		wantStatus  int
		wantCode    string
		wantSaves   int
		wantDeletes int
		wantDoc     bool
	}{
		{
			name: "erased before write", erased: func(f *acctFixture) { f.probe.erased = true },
			wantStatus: http.StatusGone, wantCode: "4010", wantSaves: 0,
		},
		{
			name: "erased after write", erased: func(f *acctFixture) { f.users.afterSave = func() { f.probe.erased = true } },
			wantStatus: http.StatusGone, wantCode: "4010", wantSaves: 1, wantDeletes: 1,
		},
		{
			name: "erased after write, undo fails", erased: func(f *acctFixture) { f.users.afterSave = func() { f.probe.erased = true } },
			deleteErr: errors.New("delete down"), wantStatus: http.StatusInternalServerError, wantCode: "5002", wantSaves: 1, wantDeletes: 1, wantDoc: true,
		},
		{
			name: "save error with mark hit still undoes", erased: func(f *acctFixture) { f.users.afterSave = func() { f.probe.erased = true } },
			saveErr: errors.New("write failed"), wantStatus: http.StatusGone, wantCode: "4010", wantSaves: 1, wantDeletes: 1,
		},
		{
			name: "save error without mark is 5001", erased: func(*acctFixture) {},
			saveErr: errors.New("write failed"), wantStatus: http.StatusInternalServerError, wantCode: "5001", wantSaves: 1,
		},
		{
			name: "not erased writes normally", erased: func(*acctFixture) {},
			wantStatus: http.StatusOK, wantSaves: 1, wantDoc: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := acctNewFixture(t)
			f.users.saveErr, f.users.deleteErr = tc.saveErr, tc.deleteErr
			tc.erased(f)
			rec := f.do(http.MethodPost, acctSyncPath, "good-U1", "")
			if rec.Code != tc.wantStatus || (tc.wantCode != "" && acctErrCode(t, rec) != tc.wantCode) {
				t.Fatalf("got %d %s, want %d/%s", rec.Code, rec.Body.String(), tc.wantStatus, tc.wantCode)
			}
			if f.users.saves != tc.wantSaves || f.users.deletes != tc.wantDeletes {
				t.Errorf("saves=%d deletes=%d, want %d/%d", f.users.saves, f.users.deletes, tc.wantSaves, tc.wantDeletes)
			}
			if _, ok := f.users.docs["U1"]; ok != tc.wantDoc {
				t.Errorf("user doc present = %v, want %v", ok, tc.wantDoc)
			}
			if f.probe.calls > 0 && !f.probe.since.IsZero() {
				t.Errorf("R1 must query ErasedSince with the zero time, got %v", f.probe.since)
			}
			if f.users.deletes > 0 && f.users.deleteCtxErr != nil {
				t.Errorf("undo ctx was canceled: %v", f.users.deleteCtxErr)
			}
		})
	}
}

// A7：R1 傳零值 since，不是請求開始時間（刪除標記只比「不早於」，零值代表標記有效期間內任何刪除都擋）.
func TestAcctSyncUsesZeroSinceNotRequestTime(t *testing.T) {
	f := acctNewFixture(t)
	f.probe.markedAt = fixedNow.Add(-time.Minute) // 刪除發生在請求開始之前
	rec := f.do(http.MethodPost, acctSyncPath, "good-U1", "")
	if rec.Code != http.StatusGone {
		t.Fatalf("a deletion before the request started must still block sync: %d %s", rec.Code, rec.Body.String())
	}
	if !f.probe.since.IsZero() {
		t.Errorf("since = %v, want zero", f.probe.since)
	}
	if f.users.saves != 0 {
		t.Errorf("saves = %d, want 0", f.users.saves)
	}
}

// A9：GET /account；別人的 token 讀不到我的值；沒有文件回空三欄（200）.
func TestAcctGetAccount(t *testing.T) {
	f := acctNewFixture(t)
	f.do(http.MethodPost, acctSyncPath, "good-U1", "")
	acctGolden(t, f.do(http.MethodGet, "/api/v1/me/account", "good-U1", ""), "account.json")
	acctGolden(t, f.do(http.MethodGet, "/api/v1/me/account", "good-U2", ""), "account_empty.json")
	rec := f.do(http.MethodGet, "/api/v1/me/account?userId=U1", "good-U2", "")
	if strings.Contains(rec.Body.String(), "Name-U1") {
		t.Errorf("?userId must be ignored: %s", rec.Body.String())
	}
	f.users.findErr = acctLeakyErr("find")
	if rec := f.do(http.MethodGet, "/api/v1/me/account", "good-U1", ""); rec.Code != http.StatusInternalServerError || acctErrCode(t, rec) != "5001" {
		t.Errorf("store error: %d %s", rec.Code, rec.Body.String())
	}
}

// ---- F30-04：R3 刪除帳號 ----

// A10：以 token 的 sub 呼叫一次 EraseAll；eraser 回錯 → 500/5002；沒注入 → 503 且不執行；路徑 / query 帶別人 ID 不影響.
func TestAcctDeleteAccount(t *testing.T) {
	f := acctNewFixture(t)
	rec := f.do(http.MethodDelete, "/api/v1/me/account?userId=U2", "good-U1", "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete: %d %q", rec.Code, rec.Body.String())
	}
	if f.eraser.calls != 1 || f.eraser.userID != "U1" {
		t.Errorf("eraser calls=%d userID=%q, want 1 call for U1", f.eraser.calls, f.eraser.userID)
	}

	f.eraser.err = errors.New("erase meals")
	rec = f.do(http.MethodDelete, "/api/v1/me/account", "good-U1", "")
	if rec.Code != http.StatusInternalServerError || acctErrCode(t, rec) != "5002" {
		t.Errorf("eraser error must be 500/5002, never 204: %d %s", rec.Code, rec.Body.String())
	}

	g := acctNewFixture(t)
	g.h.WithAccountEraser(nil)
	if rec := g.do(http.MethodDelete, "/api/v1/me/account", "good-U1", ""); rec.Code != http.StatusServiceUnavailable || acctErrCode(t, rec) != "5001" {
		t.Errorf("no eraser: %d %s", rec.Code, rec.Body.String())
	}
}

// A35：請求 ctx 在 EraseAll 途中被取消，所有步驟仍完成並回 204；逾時真的存在（卡住 → 500/5002，不是 204）.
func TestAcctDeleteAccountSurvivesRequestCancelAndTimesOut(t *testing.T) {
	f := acctNewFixture(t)
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.eraser.beforeSteps = cancel // 一進 EraseAll 請求 ctx 就被取消（使用者關掉頁面）
	req := httptest.NewRequestWithContext(reqCtx, http.MethodDelete, "/api/v1/me/account", http.NoBody)
	req.Header.Set("Authorization", "Bearer good-U1")
	rec := httptest.NewRecorder()
	f.e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if f.eraser.stepsRun != f.eraser.steps {
		t.Errorf("only %d/%d steps ran with a live ctx (errs=%v)", f.eraser.stepsRun, f.eraser.steps, f.eraser.ctxErrs)
	}
	for i, e := range f.eraser.ctxErrs {
		if e != nil {
			t.Errorf("step %d saw ctx error %v", i, e)
		}
	}

	g := acctNewFixture(t)
	g.eraser.block = true
	g.h.eraseTimeout = 30 * time.Millisecond
	rec = g.do(http.MethodDelete, "/api/v1/me/account", "good-U1", "")
	if rec.Code != http.StatusInternalServerError || acctErrCode(t, rec) != "5002" {
		t.Errorf("a stuck erase must time out into 500/5002: %d %s", rec.Code, rec.Body.String())
	}
}

// 預設逾時是 20 秒（小於平台關機寬限 30 秒）.
func TestAcctDefaultEraseTimeoutIs20Seconds(t *testing.T) {
	if defaultEraseTimeout != 20*time.Second {
		t.Errorf("defaultEraseTimeout = %v", defaultEraseTimeout)
	}
}

// ---- F30-08：log 去識別（A21）----

// 跑完 R1 / R2 / R3 / 通知 / 月資料的儲存錯誤路徑：log 不含 userID、email、主機:埠、名稱.
func TestAcctErrorLogsNeverLeakPersonalData(t *testing.T) {
	f := acctNewFixture(t)
	buf := acctCaptureLog(t)
	f.users.saveErr = acctLeakyErr("save profile")
	f.users.findErr = acctLeakyErr("find user")
	f.eraser.err = acctLeakyErr("erase")
	f.subStore.listErr = acctLeakyErr("list")
	f.subStore.upsertErr = acctLeakyErr("upsert")
	f.subStore.deleteErr = acctLeakyErr("delete")
	f.users.deleteErr = acctLeakyErr("undo")

	calls := []struct{ method, path, body string }{
		{http.MethodPost, acctSyncPath, ""},
		{http.MethodGet, "/api/v1/me/account", ""},
		{http.MethodDelete, "/api/v1/me/account", ""},
		{http.MethodGet, "/api/v1/me/alerts", ""},
		{http.MethodPut, "/api/v1/me/alerts/weather_warning", `{"regions":["台北市"]}`},
		{http.MethodPut, "/api/v1/me/alerts/earthquake_tsunami", ""},
		{http.MethodDelete, "/api/v1/me/alerts/weather_warning", ""},
	}
	for _, c := range calls {
		if rec := f.do(c.method, c.path, "secret-U1", c.body); rec.Code < 500 {
			t.Errorf("%s %s: status %d, want a 5xx error path", c.method, c.path, rec.Code)
		}
	}
	// 補刪失敗路徑：寫入後標記命中、Delete 回含個資的錯誤.
	f.users.afterSave = func() { f.probe.erased = true }
	f.do(http.MethodPost, acctSyncPath, "secret-U1", "")

	out := buf.String()
	if out == "" {
		t.Fatal("expected error logs from the failure paths")
	}
	for _, leak := range []string{acctUID, "a@b.test", "10.0.0.5:27017", "10.0.0.5", acctSecretName, acctSecretEmail, "secret.png"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaks %q:\n%s", leak, out)
		}
	}
}

// 成功的路徑也不把名稱 / email / 頭像寫進 log.
func TestAcctSuccessfulSyncLogsNoProfile(t *testing.T) {
	f := acctNewFixture(t)
	buf := acctCaptureLog(t)
	if rec := f.do(http.MethodPost, acctSyncPath, "secret-U1", ""); rec.Code != http.StatusOK {
		t.Fatalf("sync: %d", rec.Code)
	}
	for _, leak := range []string{acctSecretName, acctSecretEmail, "secret.png", "U1"} {
		if strings.Contains(buf.String(), leak) {
			t.Errorf("log leaks %q: %s", leak, buf.String())
		}
	}
}

// 任何 handler 都不讀 query / body / path 的 userId（身分只來自驗過的 token）：原始碼層面的檢查.
func TestAcctHandlersNeverReadUserIDFromRequest(t *testing.T) {
	for _, file := range []string{"account.go", "alerts.go", "month.go"} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{`QueryParam("userId")`, `QueryParam("user_id")`, `Param("userId")`, `FormValue("userId")`} {
			if strings.Contains(string(b), banned) {
				t.Errorf("%s reads %s from the request", file, banned)
			}
		}
	}
}
