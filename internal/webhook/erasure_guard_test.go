package webhook

import (
	"bytes"
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/geocoding"
	"chatbot-go/internal/models"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/platform/background"
	"chatbot-go/internal/recentmeal"
	"chatbot-go/internal/storage/database/alertsub"
	"chatbot-go/internal/storage/database/meal"
	"chatbot-go/internal/storage/database/profile"
	userstore "chatbot-go/internal/storage/database/user"
	weatherstore "chatbot-go/internal/storage/database/weather"
	"chatbot-go/internal/storage/database/workout"
	"chatbot-go/internal/storage/storagetest"
	"chatbot-go/internal/weather"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// guardUID LINE userID 的真實格式（U 加 32 位小寫 hex）；所有注入的錯誤字串都內含它，
// 讓 log 若沒遮蔽就會外洩.
const guardUID = "U0123456789abcdef0123456789abcdef"

// ctxRedis [F291-01] 照 go-redis v9.23.0 行為的假 Redis：ctx 已取消時不送指令、直接回 ctx.Err()
// （實測 internal/pool/pool.go 第 1206-1209 行 waitTurn 的 case <-ctx.Done()）. 不連線.
// onSet 在 SET 存入「之後」呼叫，回錯就讓這次 SET 回錯（資料已存）；onDel 在 DEL「之前」呼叫，回錯就不刪.
type ctxRedis struct {
	mu    sync.Mutex
	data  map[string]string
	onSet func(ctx context.Context, key string) error
	onDel func(ctx context.Context, key string) error
	// onGet [SEC29-01] 在 GET 查表「之前」呼叫，回錯就讓這次 GET 回錯.
	onGet func(ctx context.Context, key string) error
}

func newCtxRedis() *ctxRedis { return &ctxRedis{data: map[string]string{}} }

func (f *ctxRedis) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.data[key]
	return ok
}

func (f *ctxRedis) get(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.data[key]
	return v, ok
}

func (f *ctxRedis) put(key, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[key] = val
}

func (f *ctxRedis) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("dial blocked") }
}

func (f *ctxRedis) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (f *ctxRedis) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := ctx.Err(); err != nil {
			cmd.SetErr(err)
			return err
		}
		args := cmd.Args()
		name := strings.ToLower(hookArgStr(args[0]))
		switch c := cmd.(type) {
		case *redis.StatusCmd:
			if name == "set" {
				key := hookArgStr(args[1])
				f.put(key, hookArgStr(args[2]))
				if f.onSet != nil {
					if err := f.onSet(ctx, key); err != nil {
						cmd.SetErr(err)
						return err
					}
				}
				c.SetVal("OK")
				return nil
			}
		case *redis.StringCmd:
			if name == "get" {
				if f.onGet != nil {
					if err := f.onGet(ctx, hookArgStr(args[1])); err != nil {
						c.SetErr(err)
						return err
					}
				}
				v, ok := f.get(hookArgStr(args[1]))
				if !ok {
					c.SetErr(redis.Nil)
					return redis.Nil
				}
				c.SetVal(v)
				return nil
			}
		case *redis.IntCmd:
			if name == "del" {
				n := 0
				for _, a := range args[1:] {
					key := hookArgStr(a)
					if f.onDel != nil {
						if err := f.onDel(ctx, key); err != nil {
							cmd.SetErr(err)
							return err
						}
					}
					f.mu.Lock()
					if _, ok := f.data[key]; ok {
						delete(f.data, key)
						n++
					}
					f.mu.Unlock()
				}
				c.SetVal(int64(n))
				return nil
			}
		}
		err := fmt.Errorf("ctxRedis: unsupported %s %T", name, cmd)
		cmd.SetErr(err)
		return err
	}
}

func (f *ctxRedis) client(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	c.AddHook(f)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// futureSent 訊息在刪除標記「之後」才送出的 ctx（標記早於 sentAt = 不算已刪除）.
func futureSent() context.Context {
	return withSentAt(context.Background(), time.Now().Add(time.Minute).UnixMilli())
}

// replyTextOf 取回覆（第一則訊息）的 text 欄位.
func replyTextOf(t *testing.T, raw string) string {
	t.Helper()
	var payload struct {
		Messages []struct {
			Text string `json:"text"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil || len(payload.Messages) == 0 {
		t.Fatalf("parse reply %q: %v", raw, err)
	}
	return payload.Messages[0].Text
}

// ---------- F291-01 helper ----------

// TestRecheckAfterWriteRunsUndoOnlyWhenErased [F291-01] 標記晚於送出時間才補刪；沒標記或標記早於送出時間不補刪.
func TestRecheckAfterWriteRunsUndoOnlyWhenErased(t *testing.T) {
	tests := []struct {
		name       string
		markFirst  bool
		sentFuture bool
		undoErr    error
		wantErased bool
		wantUndo   int
	}{
		{"mark after sent runs undo once", true, false, nil, true, 1},
		{"no mark never runs undo", false, false, nil, false, 0},
		{"mark earlier than sent never runs undo", true, true, nil, false, 0},
		{"undo error is returned with erased true", true, false, errors.New("boom"), true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewWebhookHandler(nil, nil, nil, nil, nil)
			ctx := sentMinuteAgo()
			if tt.sentFuture {
				ctx = futureSent()
			}
			if tt.markFirst {
				h.erased.mark(context.Background(), guardUID)
			}
			calls := 0

			erased, err := h.recheckAfterWrite(ctx, guardUID, func(context.Context) error {
				calls++
				return tt.undoErr
			})

			if erased != tt.wantErased || calls != tt.wantUndo || !errors.Is(err, tt.undoErr) {
				t.Errorf("erased=%v undoCalls=%d err=%v, want erased=%v undoCalls=%d err=%v",
					erased, calls, err, tt.wantErased, tt.wantUndo, tt.undoErr)
			}
		})
	}
}

// TestRecheckAfterWriteSurvivesCancelledContext [F291-01] 請求 ctx 已取消、標記只在 Redis：複查仍讀到標記，補刪收到沒取消的 ctx.
func TestRecheckAfterWriteSurvivesCancelledContext(t *testing.T) {
	fake := newCtxRedis()
	h := NewWebhookHandler(nil, nil, nil, nil, nil)
	h.erased = newErasures(fake.client(t)) // 本地沒有標記
	fake.put(erasureKeyPrefix+guardUID, strconv.FormatInt(time.Now().UnixMilli(), 10))
	ctx, cancel := context.WithCancel(sentMinuteAgo())
	cancel()
	var undoCtxErr error
	calls := 0

	erased, err := h.recheckAfterWrite(ctx, guardUID, func(c context.Context) error {
		calls++
		undoCtxErr = c.Err()
		return nil
	})

	if !erased || err != nil || calls != 1 {
		t.Fatalf("erased=%v err=%v undoCalls=%d, want true nil 1", erased, err, calls)
	}
	if undoCtxErr != nil {
		t.Errorf("undo ctx must not be canceled, Err() = %v", undoCtxErr)
	}
}

// TestRecheckAfterWriteUndoContextHasFiveSecondDeadline [F291-01] 補刪的 ctx 有期限，且不超過 5 秒.
func TestRecheckAfterWriteUndoContextHasFiveSecondDeadline(t *testing.T) {
	h := NewWebhookHandler(nil, nil, nil, nil, nil)
	h.erased.mark(context.Background(), guardUID)
	var deadline time.Time
	var ok bool

	_, _ = h.recheckAfterWrite(sentMinuteAgo(), guardUID, func(c context.Context) error {
		deadline, ok = c.Deadline()
		return nil
	})

	if !ok {
		t.Fatal("undo ctx must carry a deadline")
	}
	if remain := time.Until(deadline); remain <= 0 || remain > 5*time.Second {
		t.Errorf("deadline remaining = %v, want in (0, 5s]", remain)
	}
}

// TestRecheckAfterWriteLogsUndoFailureRedacted [F291-01] 補刪失敗記 Error（不是 Warn），錯誤字串裡的 userID 被遮蔽.
func TestRecheckAfterWriteLogsUndoFailureRedacted(t *testing.T) {
	buf := captureSlog(t)
	h := NewWebhookHandler(nil, nil, nil, nil, nil)
	h.erased.mark(context.Background(), guardUID)
	undoErr := errors.New("delete failed for " + guardUID)

	erased, err := h.recheckAfterWrite(sentMinuteAgo(), guardUID, func(context.Context) error { return undoErr })

	if !erased || !errors.Is(err, undoErr) {
		t.Fatalf("erased=%v err=%v, want true and the undo error", erased, err)
	}
	assertUndoFailureLogged(t, buf)
}

// assertUndoFailureLogged 補刪失敗的 log：至少一筆、級別 ERROR、userID 遮成 U***、不含原 userID.
func assertUndoFailureLogged(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	recs := recordsByMsg(t, buf, "undo write after data erasure")
	if len(recs) == 0 {
		t.Fatalf("no undo failure log, output: %s", buf.String())
	}
	for _, rec := range recs {
		if rec["level"] != "ERROR" {
			t.Errorf("undo failure level = %v, want ERROR", rec["level"])
		}
		got := fmt.Sprint(rec["error"])
		if strings.Contains(got, guardUID) || !strings.Contains(got, "U***") {
			t.Errorf("undo failure error = %q, want the userID masked as U***", got)
		}
	}
}

// TestReplyErasedAfterWriteTruthfulMessage [F291-01] 補刪成功回已刪除；補刪失敗回刪除失敗，絕不回已刪除.
func TestReplyErasedAfterWriteTruthfulMessage(t *testing.T) {
	tests := []struct {
		name    string
		undoErr error
		want    string
		notWant string
	}{
		{"undo succeeded says erased", nil, erasedMessage, eraseFailedMessage},
		{"undo failed says erase failed", errors.New("delete failed"), eraseFailedMessage, erasedMessage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newLineAPI(t, nil)
			h := NewWebhookHandler(api.client, nil, nil, nil, nil)

			h.replyErasedAfterWrite(context.Background(), "tok", tt.undoErr)

			reply := api.lastReply(t)
			if !strings.Contains(reply, tt.want) || strings.Contains(reply, tt.notWant) {
				t.Errorf("reply = %s, want %q and not %q", reply, tt.want, tt.notWant)
			}
		})
	}
}

// ---------- F291-02 標記的 Redis 讀寫不受請求 ctx 影響 ----------

// TestErasuresMarkWritesRedisWithCancelledContext [F291-02] 請求 ctx 已取消，mark 仍把標記寫進 Redis（值為毫秒時間）.
func TestErasuresMarkWritesRedisWithCancelledContext(t *testing.T) {
	fake := newCtxRedis()
	e := newErasures(fake.client(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := time.Now().Add(-time.Second).UnixMilli()

	e.mark(ctx, guardUID)

	val, ok := fake.get(erasureKeyPrefix + guardUID)
	if !ok {
		t.Fatal("erased:<userID> was not written to Redis with a canceled ctx")
	}
	ms, err := strconv.ParseInt(val, 10, 64)
	if err != nil || ms < before || ms > time.Now().UnixMilli() {
		t.Errorf("stored value = %q (err %v), want a millisecond timestamp of now", val, err)
	}
}

// TestErasedSinceReadsRedisWithCancelledContext [F291-02] 本地沒標記、Redis 有：ctx 已取消仍讀到；Redis 沒標記回 false.
func TestErasedSinceReadsRedisWithCancelledContext(t *testing.T) {
	tests := []struct {
		name   string
		marked bool
		want   bool
	}{
		{"marker only in redis is found", true, true},
		{"no marker anywhere is not erased", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newCtxRedis()
			e := newErasures(fake.client(t)) // 本地沒有標記
			if tt.marked {
				fake.put(erasureKeyPrefix+guardUID, strconv.FormatInt(time.Now().UnixMilli(), 10))
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			got := e.erasedSince(ctx, guardUID, time.Now().Add(-time.Minute))

			if got != tt.want {
				t.Errorf("erasedSince = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestErasedSincePrefersNewerRedisMark [SEC29-01][F292-01] 本機只有較舊的標記、Redis 有較新的：以 Redis 為準.
func TestErasedSincePrefersNewerRedisMark(t *testing.T) {
	const uid = "U-newer-mark"
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	fake := newCtxRedis()
	e := newErasures(fake.client(t))
	e.now = func() time.Time { return base }
	e.local.Set(uid, base.Add(-5*time.Minute))
	fake.put(erasureKeyPrefix+uid, strconv.FormatInt(base.Add(-time.Minute).UnixMilli(), 10))

	if !e.erasedSince(context.Background(), uid, base.Add(-3*time.Minute)) {
		t.Error("newer Redis mark ignored: a message sent between the two erasures was not treated as erased")
	}
}

// TestErasedSinceLogsRedisReadError [SEC29-01][F292-01] Redis 讀標記出錯時記 Warn（只帶遮蔽後的錯誤）並沿用本機結果.
func TestErasedSinceLogsRedisReadError(t *testing.T) {
	fake := newCtxRedis()
	fake.onGet = func(context.Context, string) error {
		return errors.New("dial tcp 10.0.0.1:6379: i/o timeout " + guardUID)
	}
	e := newErasures(fake.client(t))
	buf := captureSlog(t)

	if e.erasedSince(context.Background(), guardUID, time.Now().Add(-time.Minute)) {
		t.Error("erasedSince = true with no mark and a failing Redis, want false")
	}

	out := buf.String()
	if !strings.Contains(out, "read data erasure mark") {
		t.Errorf("log %q does not contain the Warn message", out)
	}
	for _, leak := range []string{"10.0.0.1", guardUID} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaks %q: %s", leak, out)
		}
	}
}

// ---------- SEC29-03 整則事件共用的刪除複查預算 ----------

// shortenErasureBudget [SEC29-03] 把預算暫時改成 d（t.Cleanup 還原）；改預算的測試 NEVER 呼叫 t.Parallel.
func shortenErasureBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := erasureBudget
	erasureBudget = d
	t.Cleanup(func() { erasureBudget = prev })
}

// waitDone 等 ctx 結束最多 1 秒；沒結束回 false.
func waitDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(time.Second):
		return false
	}
}

// TestErasureBudgetBoundsDetachedOpsAfterCancel [SEC29-03][F292-03] 請求取消後，複查與補刪（含巢狀）不立刻被切，但在預算內全部收掉.
func TestErasureBudgetBoundsDetachedOpsAfterCancel(t *testing.T) {
	shortenErasureBudget(t, 50*time.Millisecond)
	parent, cancel := context.WithCancel(context.Background())
	ectx, done := withErasureBudget(parent)
	defer done()
	op, opCancel := erasureOpCtx(ectx)
	defer opCancel()
	nested, nCancel := erasureOpCtx(op)
	defer nCancel()

	cancel()

	if op.Err() != nil {
		t.Fatal("detached op cancelled together with the request")
	}
	if !waitDone(op) {
		t.Fatal("detached op still running after the erasure budget")
	}
	if !waitDone(nested) {
		t.Fatal("nested detached op still running after the erasure budget")
	}
}

// TestEventContextCarriesErasureBudget [SEC29-03][F292-03] eventContext 回傳的 ctx 帶著預算.
func TestEventContextCarriesErasureBudget(t *testing.T) {
	shortenErasureBudget(t, 50*time.Millisecond)
	parent, cancel := context.WithCancel(context.Background())
	ectx, done := eventContext(parent)
	defer done()
	op, c := erasureOpCtx(ectx)
	defer c()

	cancel()

	if !waitDone(op) {
		t.Fatal("event context has no erasure budget")
	}
}

// TestCancelGraceCoversErasureBudget [SEC29-03][F292-03] 關機寬限必須大於預設的刪除複查預算.
func TestCancelGraceCoversErasureBudget(t *testing.T) {
	if background.CancelGrace <= defaultErasureBudget {
		t.Fatalf("CancelGrace %v must exceed the default erasure budget %v", background.CancelGrace, defaultErasureBudget)
	}
}

// TestProcessEventsAppliesErasureBudget [SEC29-03][F292-03] 經 processEvents：請求在寫入使用者文件時被取消，
// 寫後再查讀標記卡住，也只卡預算那麼久，不是整整 erasureOpTimeout.
func TestProcessEventsAppliesErasureBudget(t *testing.T) {
	shortenErasureBudget(t, 50*time.Millisecond)
	e := newGuardEnv(t)
	e.h.erased = newErasures(e.rds.client(t))
	e.rds.onGet = func(ctx context.Context, key string) error {
		if strings.HasPrefix(key, erasureKeyPrefix) {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	parent, cancel := context.WithCancel(context.Background())
	e.beforeWrite = func(kind string) {
		if kind == "user" {
			cancel()
		}
	}

	start := time.Now()
	e.h.processEvents(parent, webhookBody(t, textEvent(map[string]string{"type": "user", "userId": guardUID}, "說明")))

	if time.Since(start) >= time.Second {
		t.Fatalf("processEvents took %v after cancel; erasure budget not applied", time.Since(start))
	}
}

// TestProcessEventsStopsAfterDeadline [SEC29-03][F292-03] ctx 已結束時，剩下的事件不再處理，只記筆數.
func TestProcessEventsStopsAfterDeadline(t *testing.T) {
	api := newLineAPI(t, nil)
	h := NewWebhookHandlerWithOptions(api.client, newFakeUserRepo(), weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil,
		conversation.NewMemoryManager(), panicGeocoder{}, nil)
	buf := captureSlog(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h.processEvents(ctx, webhookBody(t, textEvent(fromUser, "說明")))

	out := buf.String()
	if !strings.Contains(out, "webhook events dropped after deadline") || !strings.Contains(out, `"remaining":1`) {
		t.Errorf("log %q must report the dropped events", out)
	}
	if len(api.replies) != 0 {
		t.Errorf("%d replies sent after the deadline, want 0", len(api.replies))
	}
}

// ---------- 寫入點測試夾具 ----------

// guardEnv 一個寫入點測試用的完整 handler 與所有可注入的 fake；每個子測試各自新建，不共用.
// fireOn：該種類的寫入「當下」記下刪除標記（模擬檢查與寫入之間完成刪除）；errOn：該種類的寫入先存值再回錯；
// failUndo：該種類的補刪回錯（補刪的 DEL 或 Delete 不執行）；undoCalls：各種類補刪被呼叫的次數.
type guardEnv struct {
	t     *testing.T
	api   *lineAPI
	h     *WebhookHandler
	rds   *ctxRedis
	conv  *conversation.Manager
	ai    *mealCompleter
	qa    *qaCompleter
	geo   *guardGeocoder
	hist  *guardHistory
	users *guardUsers
	subs  *guardSubs

	workouts *guardWorkouts
	profiles *guardProfiles
	meals    *guardMeals

	// beforeWrite [SEC29-03] 非 nil 時，每次寫入發生前先以寫入種類呼叫它（測試用來在寫入當下取消請求）.
	beforeWrite func(kind string)

	fireOn    string
	errOn     string
	failUndo  map[string]bool
	undoCalls map[string]int
	deleteAll int
}

type guardGeocoder struct{ results []geocoding.GeocodeResult }

func (g *guardGeocoder) Geocode(context.Context, string) ([]geocoding.GeocodeResult, error) {
	return g.results, nil
}

// write 一次寫入發生：種類符合 fireOn 就記下刪除標記；符合 errOn 回錯（呼叫端已把資料存下）.
func (e *guardEnv) write(ctx context.Context, kind string) error {
	if e.beforeWrite != nil {
		e.beforeWrite(kind)
	}
	if kind == e.fireOn {
		e.h.erased.mark(ctx, guardUID)
	}
	if kind == e.errOn {
		return errors.New("injected write failure for " + guardUID)
	}
	return nil
}

// undo 一次補刪被呼叫：計數；該種類設為失敗就回錯.
func (e *guardEnv) undo(kind string) error {
	e.undoCalls[kind]++
	if e.failUndo[kind] {
		return errors.New("injected undo failure for " + guardUID)
	}
	return nil
}

func redisKind(key string) string {
	switch {
	case strings.HasPrefix(key, "conv:"):
		return "conv"
	case strings.HasPrefix(key, "loc2:"), strings.HasPrefix(key, "loc:"):
		return "loc"
	case strings.HasPrefix(key, recentmeal.KeyPrefix):
		return "recent"
	}
	return ""
}

func newGuardEnv(t *testing.T) *guardEnv {
	t.Helper()
	e := &guardEnv{
		t: t, failUndo: map[string]bool{}, undoCalls: map[string]int{},
		rds: newCtxRedis(), ai: &mealCompleter{}, qa: &qaCompleter{answer: "晚餐可以選雞胸肉沙拉。"},
		geo: &guardGeocoder{},
	}
	e.rds.onSet = func(ctx context.Context, key string) error {
		if kind := redisKind(key); kind != "" {
			return e.write(ctx, kind)
		}
		return nil
	}
	e.rds.onDel = func(_ context.Context, key string) error {
		if kind := redisKind(key); kind != "" {
			return e.undo(kind)
		}
		return nil
	}
	client := e.rds.client(t)
	e.api = newLineAPI(t, jpegHeader)
	e.conv = conversation.NewManager(client)
	e.workouts = &guardWorkouts{Workouts: &storagetest.Workouts{}, e: e}
	e.profiles = &guardProfiles{Profiles: storagetest.NewProfiles(), e: e}
	e.meals = &guardMeals{Meals: &storagetest.Meals{}, e: e}
	e.users = &guardUsers{e: e, docs: map[string]bool{}}
	e.subs = &guardSubs{e: e, saved: map[string]alertsub.AlertSubscription{}}
	e.hist = &guardHistory{e: e, repliesAtClear: -1}
	repo := &placeWeatherRepo{districts: numberedDistricts(3)}
	lookup := weather.NewLookup(repo, nil, 60)
	e.h = NewWebhookHandlerWithOptions(e.api.client, e.users, lookup, e.subs, e.conv, e.geo, repo).
		WithNutrition(nutrition.NewAnalyzer(e.ai), e.meals, nil).
		WithFitness(e.workouts, e.profiles, nil).
		WithAssistant(assistant.New(&assistant.Config{Completer: e.qa, History: e.hist}), weather.NewUserLocations(client, lookup))
	e.h.recent = recentmeal.New(client)
	e.h.erased = newErasures(nil) // 標記只放本地：fire 不經過 Redis
	return e
}

type guardWorkouts struct {
	*storagetest.Workouts
	e *guardEnv
}

func (g *guardWorkouts) Insert(ctx context.Context, w *workout.Workout) error {
	if err := g.Workouts.Insert(ctx, w); err != nil {
		return err
	}
	return g.e.write(ctx, "workout")
}

func (g *guardWorkouts) DeleteByID(ctx context.Context, userID, id string) (bool, error) {
	if err := g.e.undo("workout"); err != nil {
		return false, err
	}
	return g.Workouts.DeleteByID(ctx, userID, id)
}

func (g *guardWorkouts) DeleteAllByUser(ctx context.Context, userID string) error {
	g.e.deleteAll++
	return g.Workouts.DeleteAllByUser(ctx, userID)
}

type guardProfiles struct {
	*storagetest.Profiles
	e *guardEnv
}

func (g *guardProfiles) Merge(ctx context.Context, patch *profile.Profile) (*profile.Profile, error) {
	p, err := g.Profiles.Merge(ctx, patch)
	if err != nil {
		return nil, err
	}
	if err := g.e.write(ctx, "profile"); err != nil {
		return nil, err
	}
	return p, nil
}

func (g *guardProfiles) Delete(ctx context.Context, userID string) error {
	if err := g.e.undo("profile"); err != nil {
		return err
	}
	return g.Profiles.Delete(ctx, userID)
}

type guardMeals struct {
	*storagetest.Meals
	e *guardEnv
}

func (g *guardMeals) Insert(ctx context.Context, m *meal.Meal) error {
	if err := g.Meals.Insert(ctx, m); err != nil {
		return err
	}
	return g.e.write(ctx, "meal")
}

func (g *guardMeals) DeleteByID(ctx context.Context, userID, id string) (bool, error) {
	if err := g.e.undo("meal"); err != nil {
		return false, err
	}
	return g.Meals.DeleteByID(ctx, userID, id)
}

func (g *guardMeals) DeleteAllByUser(ctx context.Context, userID string) error {
	g.e.deleteAll++
	return g.Meals.DeleteAllByUser(ctx, userID)
}

type guardUsers struct {
	e    *guardEnv
	mu   sync.Mutex
	docs map[string]bool
}

func (g *guardUsers) has(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.docs[id]
}

func (g *guardUsers) Upsert(ctx context.Context, u *userstore.User) error {
	g.mu.Lock()
	g.docs[u.ID] = true
	g.mu.Unlock()
	return g.e.write(ctx, "user")
}

func (g *guardUsers) Delete(_ context.Context, id string) error {
	if err := g.e.undo("user"); err != nil {
		return err
	}
	g.mu.Lock()
	delete(g.docs, id)
	g.mu.Unlock()
	return nil
}

type guardSubs struct {
	e     *guardEnv
	saved map[string]alertsub.AlertSubscription
	// deleteByUser DeleteByUser 被呼叫的次數（補刪不得用整人刪除）.
	deleteByUser int
}

func (g *guardSubs) Upsert(ctx context.Context, sub *alertsub.AlertSubscription) error {
	g.saved[sub.ID] = *sub
	return g.e.write(ctx, "sub")
}

func (g *guardSubs) Delete(_ context.Context, id string) error {
	if err := g.e.undo("sub"); err != nil {
		return err
	}
	delete(g.saved, id)
	return nil
}

func (g *guardSubs) DeleteByUser(context.Context, string) error {
	g.deleteByUser++
	return nil
}

// guardHistory 問答紀錄：Append 當下可記下刪除標記；Clear 記下被呼叫時已送出幾則回覆（驗證先補刪、後回覆）.
type guardHistory struct {
	e              *guardEnv
	mu             sync.Mutex
	turns          []assistant.Turn
	repliesAtClear int
}

func (g *guardHistory) Recent(context.Context, string) []assistant.Turn {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]assistant.Turn(nil), g.turns...)
}

func (g *guardHistory) Append(ctx context.Context, _ string, t assistant.Turn) {
	g.mu.Lock()
	g.turns = append(g.turns, t)
	g.mu.Unlock()
	_ = g.e.write(ctx, "history") // Append 沒有回傳值，不會回錯
}

func (g *guardHistory) Clear(context.Context, string) error {
	g.repliesAtClear = len(g.e.api.replies)
	if err := g.e.undo("qa"); err != nil {
		return err
	}
	g.mu.Lock()
	g.turns = nil
	g.mu.Unlock()
	return nil
}

// guardPoint 一個寫入點的測試規格；runGuard 依它跑固定的幾個子測試.
type guardPoint struct {
	kind     string // 寫入的種類（fireOn / errOn 用）
	variants []guardVariant
	setup    func(e *guardEnv)
	act      func(e *guardEnv, ctx context.Context)
	present  func(e *guardEnv) bool // 這次寫入的資料還在嗎
	ok       string                 // 正常路徑的回覆內容
	canErr   bool                   // 這個點的寫入會回錯（判定見開工單差異第 8 條）
	errReply string                 // 寫入回錯、沒被刪除時的回覆（silent 的點用 ok）
	// silent：這個點自己不回覆（saveUser），命中與補刪失敗時回覆都維持 ok，且不得出現已刪除或刪除失敗.
	silent bool
	// undoCallsUnreliable：正常路徑本來就會呼叫補刪用的方法（finishRegionSelection 成功時也清對話狀態），不查「沒命中時補刪 0 次」.
	undoCallsUnreliable bool
	undo                []string // 補刪的步驟（種類）；兩步時各有 first_step / second_step 子測試
	// 預設：between 回 erasedMessage 且不含 ok；undo_fails 回 eraseFailedMessage 且不含 erasedMessage.
	betweenWant, betweenNot []string
	undoWant, undoNot       []string
	afterBetween            func(t *testing.T, e *guardEnv)
	afterUndoFails          func(t *testing.T, e *guardEnv)
}

type guardVariant struct {
	name string
	act  func(e *guardEnv, ctx context.Context)
	// event 以 LINE 事件進場（handleEventSafely）：事件時間 tsMS 取自這個子測試的送出時間，ctx 不帶 sentAt，
	// 好讓 saveUser 的事件時間只能來自事件本身.
	event func(e *guardEnv, tsMS int64)
	ok    string
}

// betweenExpect 命中且補刪成功時，回覆該含與不該含的字串.
func (gp *guardPoint) betweenExpect(ok string) (want, notWant []string) {
	if gp.silent {
		return []string{ok}, []string{erasedMessage, eraseFailedMessage}
	}
	want, notWant = gp.betweenWant, gp.betweenNot
	if want == nil {
		want = []string{erasedMessage}
	}
	if notWant == nil {
		notWant = []string{ok}
	}
	return want, notWant
}

func (gp *guardPoint) newEnv(t *testing.T) *guardEnv {
	t.Helper()
	e := newGuardEnv(t)
	if gp.setup != nil {
		gp.setup(e)
	}
	return e
}

func assertReply(t *testing.T, reply string, want, notWant []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(reply, w) {
			t.Errorf("reply missing %q: %s", w, reply)
		}
	}
	for _, n := range notWant {
		if strings.Contains(reply, n) {
			t.Errorf("reply must not contain %q: %s", n, reply)
		}
	}
}

// runGuard 每個寫入點固定的子測試：erased_between_write_and_check、erased_before_sent、undo_fails（兩步補刪為
// undo_fails_first_step / undo_fails_second_step）、write_error_then_erased（寫入會回錯的點）.
func runGuard(t *testing.T, gp *guardPoint) {
	t.Helper()
	variants := gp.variants
	if len(variants) == 0 {
		variants = []guardVariant{{}}
	}
	for _, v := range variants {
		run := func(t *testing.T) { gp.runVariant(t, v) }
		if v.name == "" {
			run(t)
		} else {
			t.Run(v.name, run)
		}
	}
}

func (gp *guardPoint) runVariant(t *testing.T, v guardVariant) {
	act, ok := gp.act, gp.ok
	if v.act != nil {
		act = v.act
	}
	if v.event != nil {
		act = func(e *guardEnv, ctx context.Context) { v.event(e, sentAt(ctx).UnixMilli()) }
	}
	if v.ok != "" {
		ok = v.ok
	}

	t.Run("erased_between_write_and_check", func(t *testing.T) {
		e := gp.newEnv(t)
		e.fireOn = gp.kind

		act(e, sentMinuteAgo())

		reply := e.api.lastReply(t)
		want, notWant := gp.betweenExpect(ok)
		assertReply(t, reply, want, notWant)
		if gp.present(e) {
			t.Error("the data written after the erasure must be removed")
		}
		for _, step := range gp.undo {
			if e.undoCalls[step] == 0 {
				t.Errorf("undo step %q was never run", step)
			}
		}
		if e.deleteAll != 0 || e.subs.deleteByUser != 0 {
			t.Errorf("undo must only remove what this write created: DeleteAllByUser=%d DeleteByUser=%d", e.deleteAll, e.subs.deleteByUser)
		}
		if gp.afterBetween != nil {
			gp.afterBetween(t, e)
		}
	})

	t.Run("erased_before_sent", func(t *testing.T) {
		e := gp.newEnv(t)
		e.h.erased.mark(context.Background(), guardUID) // 標記早於這則訊息的送出時間

		act(e, futureSent())

		assertReply(t, e.api.lastReply(t), []string{ok}, []string{erasedMessage, eraseFailedMessage})
		if !gp.present(e) {
			t.Error("a write sent after the erasure must be kept")
		}
		for step, n := range e.undoCalls {
			if n != 0 && !gp.undoCallsUnreliable {
				t.Errorf("undo step %q ran %d times, want 0", step, n)
			}
		}
		if e.deleteAll != 0 {
			t.Errorf("DeleteAllByUser = %d, want 0", e.deleteAll)
		}
	})

	failUndo := func(name, failing, other string) {
		t.Run(name, func(t *testing.T) {
			buf := captureSlog(t)
			e := gp.newEnv(t)
			e.fireOn = gp.kind
			e.failUndo[failing] = true

			act(e, sentMinuteAgo())

			want, notWant := gp.undoWant, gp.undoNot
			if gp.silent {
				want, notWant = []string{ok}, []string{erasedMessage, eraseFailedMessage}
			}
			if want == nil {
				want = []string{eraseFailedMessage}
			}
			if notWant == nil {
				notWant = []string{erasedMessage}
			}
			assertReply(t, e.api.lastReply(t), want, notWant)
			assertUndoFailureLogged(t, buf)
			if other != "" && e.undoCalls[other] == 0 {
				t.Errorf("the other undo step %q must still run when %q fails", other, failing)
			}
			if gp.afterUndoFails != nil {
				gp.afterUndoFails(t, e)
			}
		})
	}
	if len(gp.undo) == 2 {
		failUndo("undo_fails_first_step", gp.undo[0], gp.undo[1])
		failUndo("undo_fails_second_step", gp.undo[1], gp.undo[0])
	} else {
		failUndo("undo_fails", gp.undo[0], "")
	}

	if !gp.canErr {
		return
	}
	errReply := gp.errReply
	if gp.silent {
		errReply = ok
	}
	t.Run("write_error_then_erased", func(t *testing.T) {
		e := gp.newEnv(t)
		e.fireOn, e.errOn = gp.kind, gp.kind

		act(e, sentMinuteAgo())

		want, notWant := gp.betweenExpect(ok)
		if !gp.silent {
			notWant = []string{errReply}
		}
		assertReply(t, e.api.lastReply(t), want, notWant)
		if gp.present(e) {
			t.Error("a write that errored after landing must still be removed")
		}
	})
	t.Run("write_error_not_erased", func(t *testing.T) {
		e := gp.newEnv(t)
		e.errOn = gp.kind

		act(e, sentMinuteAgo())

		assertReply(t, e.api.lastReply(t), []string{errReply}, []string{erasedMessage, eraseFailedMessage})
		for step, n := range e.undoCalls {
			if n != 0 && !gp.undoCallsUnreliable {
				t.Errorf("undo step %q ran %d times without an erasure", step, n)
			}
		}
	})
}

// ---------- 寫入點 ----------

var guardFoods = []meal.FoodItem{{Name: "牛肉麵", Portion: "1碗", Nutrients: meal.Nutrients{Calories: 650}}}

var guardForecast = &weatherstore.WeatherForecast{
	City: "台北市", District: "信義區",
	Elements: []weatherstore.ForecastElement{{ElementName: "PoP12h", Description: "12小時降雨機率", Value: "80"}},
}

// convPresent 對話狀態還在 Redis 嗎.
func convPresent(e *guardEnv) bool { return e.rds.has("conv:" + guardUID) }

// [F291-03] 對話狀態寫入點 #1 #2 #5 #7 #8 #10 #14 #15 #17 #18：寫後再查，命中就整份清掉並回已刪除.

func TestWriteGuardAskDuration(t *testing.T) {
	runGuard(t, &guardPoint{
		kind:    "conv",
		act:     func(e *guardEnv, ctx context.Context) { e.h.askDuration(ctx, guardUID, "tok", "跑步") },
		present: convPresent, ok: "跑步多久呢", undo: []string{"conv"},
	})
}

func TestWriteGuardTryAIWorkout(t *testing.T) {
	runGuard(t, &guardPoint{
		kind: "conv",
		setup: func(e *guardEnv) {
			e.h.aiParser = fitness.NewAIParser(&visionCompleter{
				answer: `{"is_workout":true,"activity":"划船","duration_min":20,"distance_km":0,"calories":0}`,
			})
		},
		act: func(e *guardEnv, ctx context.Context) {
			if !e.h.tryAIWorkout(ctx, guardUID, "下班練了二十分鐘的划槳", "tok") {
				e.t.Error("tryAIWorkout must report the message as handled")
			}
		},
		present: convPresent, ok: "要記錄這筆運動嗎", undo: []string{"conv"},
	})
}

func TestWriteGuardAskDeleteData(t *testing.T) {
	runGuard(t, &guardPoint{
		kind:    "conv",
		act:     func(e *guardEnv, ctx context.Context) { e.h.askDeleteData(ctx, guardUID, "tok") },
		present: convPresent, ok: "確定要刪除你所有的個人資料嗎", undo: []string{"conv"},
		canErr: true, errReply: "目前無法處理，請稍後再試",
		// 清掉之後再送「確認」：走一般文字處理，不是刪除.
		afterBetween: func(t *testing.T, e *guardEnv) {
			e.h.handleTextMessage(context.Background(), guardUID, "確認刪除", "tok2")
			if strings.Contains(e.api.lastReply(t), "已刪除你的所有個人資料") {
				t.Errorf("a stray confirmation must not erase anything: %s", e.api.lastReply(t))
			}
			if e.deleteAll != 0 {
				t.Errorf("DeleteAllByUser = %d, want 0", e.deleteAll)
			}
		},
	})
}

func TestWriteGuardLocationClarifyFuzzy(t *testing.T) {
	runGuard(t, &guardPoint{
		kind: "conv",
		act: func(e *guardEnv, ctx context.Context) {
			if !e.h.handleFuzzyMatch(ctx, guardUID, "測試", "tok") {
				e.t.Error("handleFuzzyMatch must report the message as handled")
			}
		},
		present: convPresent, ok: "找到多個符合的地點", undo: []string{"conv"},
	})
}

func TestWriteGuardLocationClarifyGeocode(t *testing.T) {
	runGuard(t, &guardPoint{
		kind: "conv",
		setup: func(e *guardEnv) {
			e.geo.results = []geocoding.GeocodeResult{
				{City: "台北市", District: "信義區", DisplayName: "信義區, 台北市"},
				{City: "台北市", District: "北投區", DisplayName: "北投區, 台北市"},
			}
		},
		act: func(e *guardEnv, ctx context.Context) {
			if !e.h.handleGeocodingFallback(ctx, guardUID, "某個地方", "tok") {
				e.t.Error("handleGeocodingFallback must report the message as handled")
			}
		},
		present: convPresent, ok: "找到多個符合的地點", undo: []string{"conv"},
	})
}

func TestWriteGuardMealClarifyState(t *testing.T) {
	runGuard(t, &guardPoint{
		kind:  "conv",
		setup: func(e *guardEnv) { e.ai.texts = []string{bentoClarify} },
		act: func(e *guardEnv, ctx context.Context) {
			e.h.handleTextMessage(ctx, guardUID, "晚餐吃便當", "tok")
		},
		present: convPresent, ok: "便當差很多", undo: []string{"conv"},
	})
}

func TestWriteGuardAskMealPlace(t *testing.T) {
	runGuard(t, &guardPoint{
		kind: "conv",
		setup: func(e *guardEnv) {
			e.h.recent.Remember(context.Background(), guardUID, "m1", meal.SourceText)
		},
		act:     func(e *guardEnv, ctx context.Context) { e.h.askMealPlace(ctx, guardUID, "tok") },
		present: convPresent, ok: "在哪間店吃的呢", undo: []string{"conv"},
	})
}

func TestWriteGuardReaskMealPlace(t *testing.T) {
	runGuard(t, &guardPoint{
		kind: "conv",
		act: func(e *guardEnv, ctx context.Context) {
			e.h.reaskMealPlace(ctx, guardUID, "tok", &conversation.PendingMeal{MealID: "m1"}, "連結看不出店名，請直接輸入店名")
		},
		present: convPresent, ok: "連結看不出店名", undo: []string{"conv"},
	})
}

func TestWriteGuardWeatherRegionStart(t *testing.T) {
	runGuard(t, &guardPoint{
		kind:    "conv",
		act:     func(e *guardEnv, ctx context.Context) { e.h.handleWeatherWarningSubscribe(ctx, guardUID, "tok") },
		present: convPresent, ok: "請輸入您想訂閱的縣市名稱", undo: []string{"conv"},
	})
}

func TestWriteGuardKeepRegionSelection(t *testing.T) {
	runGuard(t, &guardPoint{
		kind: "conv",
		act: func(e *guardEnv, ctx context.Context) {
			state := &conversation.State{Type: conversation.TypeWeatherRegionSelect, Regions: []string{}}
			e.h.handleRegionSelection(ctx, guardUID, "整個台北市", "tok", state)
		},
		present: convPresent, ok: "已加入「台北市」", undo: []string{"conv"},
	})
}

// [F291-04] 有 ID 的紀錄：補刪只刪這次寫的那一筆.

func TestWriteGuardRecordWorkout(t *testing.T) {
	const otherID = "other-w"
	mine := func(e *guardEnv) bool {
		for _, w := range e.workouts.Items {
			if w.ID != otherID {
				return true
			}
		}
		return false
	}
	runGuard(t, &guardPoint{
		kind: "workout",
		setup: func(e *guardEnv) {
			now := time.Now()
			if err := e.workouts.Workouts.Insert(context.Background(), &workout.Workout{
				ID: otherID, UserID: guardUID, Activity: "走路", DurationMin: 10, Calories: 30, DoneAt: now, CreateTime: now,
			}); err != nil {
				e.t.Fatalf("seed workout: %v", err)
			}
		},
		act: func(e *guardEnv, ctx context.Context) {
			e.h.recordWorkout(ctx, guardUID, "tok", &fitness.Parsed{
				Activity: fitness.ActivityByName("跑步"), DurationMin: 30, DistanceKM: 5,
			}, workout.SourceText)
		},
		present: mine, ok: "已記錄", undo: []string{"workout"},
		canErr: true, errReply: "記錄失敗",
		afterBetween: func(t *testing.T, e *guardEnv) {
			if len(e.workouts.Items) != 1 || e.workouts.Items[0].ID != otherID {
				t.Errorf("the workout recorded before the erasure must stay: %+v", e.workouts.Items)
			}
		},
	})
}

func TestWriteGuardSaveMeal(t *testing.T) {
	const otherID = "other-m"
	mine := func(e *guardEnv) bool {
		for _, m := range e.meals.Items {
			if m.ID != otherID {
				return true
			}
		}
		return false
	}
	runGuard(t, &guardPoint{
		kind: "meal",
		variants: []guardVariant{
			{name: "text", act: func(e *guardEnv, ctx context.Context) {
				e.h.saveTextMeal(ctx, guardUID, "tok", "午餐吃牛肉麵", &nutrition.TextEstimate{IsFood: true, Foods: guardFoods})
			}},
			{name: "photo", act: func(e *guardEnv, ctx context.Context) {
				e.h.recordPhotoMeal(ctx, guardUID, "tok", &nutrition.ImageAnalysis{Foods: guardFoods}, nil)
			}},
		},
		setup: func(e *guardEnv) {
			now := time.Now()
			if err := e.meals.Meals.Insert(context.Background(), &meal.Meal{
				ID: otherID, UserID: guardUID, EatenAt: now, CreateTime: now, Items: guardFoods, Source: meal.SourceText,
			}); err != nil {
				e.t.Fatalf("seed meal: %v", err)
			}
		},
		present: mine, ok: "已記錄", undo: []string{"meal", "recent"},
		canErr: true, errReply: "暫時無法儲存",
		betweenNot: []string{"已記錄", "今日累計"},
		afterBetween: func(t *testing.T, e *guardEnv) {
			if len(e.meals.Items) != 1 || e.meals.Items[0].ID != otherID {
				t.Errorf("the meal recorded before the erasure must stay: %+v", e.meals.Items)
			}
			if _, ok := e.h.recent.Get(context.Background(), guardUID); ok {
				t.Error("the recent-meal pointer must be forgotten")
			}
		},
	})
}

// TestWriteGuardSaveMealForgetsRecent [F291-04] 複查在最後一個寫入（最近一餐指標）之後：
// 指標寫入當下完成的刪除，餐點與指標都清掉.
func TestWriteGuardSaveMealForgetsRecent(t *testing.T) {
	save := func(e *guardEnv, ctx context.Context) {
		e.h.saveTextMeal(ctx, guardUID, "tok", "午餐吃牛肉麵", &nutrition.TextEstimate{IsFood: true, Foods: guardFoods})
	}
	t.Run("erased_during_recent_remember", func(t *testing.T) {
		e := newGuardEnv(t)
		e.fireOn = "recent" // 此時 Insert 已完成

		save(e, sentMinuteAgo())

		assertReply(t, e.api.lastReply(t), []string{erasedMessage}, []string{"已記錄"})
		if len(e.meals.Items) != 0 {
			t.Errorf("meal must be removed by ID: %+v", e.meals.Items)
		}
		if _, ok := e.h.recent.Get(context.Background(), guardUID); ok {
			t.Error("recent.Get must be false after the undo")
		}
		if e.undoCalls["meal"] != 1 || e.undoCalls["recent"] != 1 {
			t.Errorf("undo calls = %v, want one DeleteByID and one Forget", e.undoCalls)
		}
	})
	t.Run("erased_before_sent", func(t *testing.T) {
		e := newGuardEnv(t)
		e.h.erased.mark(context.Background(), guardUID)

		save(e, futureSent())

		assertReply(t, e.api.lastReply(t), []string{"已記錄"}, []string{erasedMessage})
		if len(e.meals.Items) != 1 {
			t.Errorf("meal must be kept: %+v", e.meals.Items)
		}
		if got, ok := e.h.recent.Get(context.Background(), guardUID); !ok || got.ID != e.meals.Items[0].ID {
			t.Errorf("recent = %+v ok=%v, want the saved meal", got, ok)
		}
	})
}

// [F291-05] 單一文件寫入點.

func TestWriteGuardUpdateProfile(t *testing.T) {
	runGuard(t, &guardPoint{
		kind: "profile",
		act: func(e *guardEnv, ctx context.Context) {
			e.h.updateProfile(ctx, guardUID, "tok", &fitness.ProfileUpdate{WeightKG: 70})
		},
		present: func(e *guardEnv) bool { _, ok := e.profiles.Items[guardUID]; return ok },
		ok:      "已更新", undo: []string{"profile"},
		canErr: true, errReply: "儲存失敗",
	})
}

// TestWriteGuardSaveUser [F291-05] 從 handleEventSafely 進場：message 事件與 follow 事件都帶 LINE 事件時間.
func TestWriteGuardSaveUser(t *testing.T) {
	event := func(typ string, ts int64) *models.Event {
		ev := &models.Event{Type: typ, ReplyToken: "tok", Timestamp: ts, Source: models.Source{Type: "user", UserID: guardUID}}
		if typ == "message" {
			ev.Message = models.Message{ID: "1", Type: "text", Text: keywordHelp}
		}
		return ev
	}
	runGuard(t, &guardPoint{
		kind: "user",
		variants: []guardVariant{
			{name: "message", ok: "我可以幫你", event: func(e *guardEnv, tsMS int64) {
				e.h.handleEventSafely(e.t.Context(), event("message", tsMS))
			}},
			{name: "follow", ok: "嗨，我是生活小幫手", event: func(e *guardEnv, tsMS int64) {
				e.h.handleEventSafely(e.t.Context(), event("follow", tsMS))
			}},
		},
		present: func(e *guardEnv) bool { return e.users.has(guardUID) },
		undo:    []string{"user"}, silent: true, canErr: true,
	})
}

func TestWriteGuardRememberLocation(t *testing.T) {
	lastLine := func(t *testing.T, e *guardEnv) {
		t.Helper()
		if text := replyTextOf(t, e.api.lastReply(t)); !strings.HasSuffix(text, eraseFailedMessage) {
			t.Errorf("the last line must be %q: %q", eraseFailedMessage, text)
		}
	}
	runGuard(t, &guardPoint{
		kind:    "loc",
		act:     func(e *guardEnv, ctx context.Context) { e.h.replyForecast(ctx, guardUID, "tok", guardForecast, "") },
		present: func(e *guardEnv) bool { return e.rds.has("loc2:" + guardUID) },
		ok:      "信義區", undo: []string{"loc"},
		// 預報是公開資料：補刪成功照回；補刪失敗照回並在最後附一行.
		betweenWant: []string{"信義區"}, betweenNot: []string{erasedMessage, eraseFailedMessage},
		undoWant: []string{"信義區", eraseFailedMessage}, undoNot: []string{erasedMessage},
		afterUndoFails: lastLine,
	})
}

func TestWriteGuardMergePhotoRecent(t *testing.T) {
	runGuard(t, &guardPoint{
		kind: "recent",
		setup: func(e *guardEnv) {
			now := time.Now()
			if err := e.meals.Meals.Insert(context.Background(), &meal.Meal{
				ID: "m1", UserID: guardUID, EatenAt: now, CreateTime: now, Items: guardFoods, Source: meal.SourceText, MealType: "lunch",
			}); err != nil {
				e.t.Fatalf("seed meal: %v", err)
			}
		},
		act: func(e *guardEnv, ctx context.Context) {
			e.h.mergePhoto(ctx, guardUID, "tok", &meal.Meal{ID: "m1"}, &nutrition.ImageAnalysis{Foods: guardFoods})
		},
		present: func(e *guardEnv) bool { return e.rds.has(recentmeal.KeyPrefix + guardUID) },
		ok:      "已用照片更新", undo: []string{"recent"},
	})
}

// [F291-06] 訂閱.

func TestWriteGuardSubscribeQuake(t *testing.T) {
	key := guardUID + "_" + alertsub.TypeEarthquakeTsunami
	runGuard(t, &guardPoint{
		kind: "sub",
		setup: func(e *guardEnv) {
			e.subs.saved[guardUID+"_other"] = alertsub.AlertSubscription{ID: guardUID + "_other", UserID: guardUID}
		},
		act:     func(e *guardEnv, ctx context.Context) { e.h.handleEarthquakeTsunamiSubscribe(ctx, guardUID, "tok") },
		present: func(e *guardEnv) bool { _, ok := e.subs.saved[key]; return ok },
		ok:      "已開啟地震及海嘯警告通知", undo: []string{"sub"},
		canErr: true, errReply: "訂閱失敗",
		afterBetween: func(t *testing.T, e *guardEnv) {
			if _, ok := e.subs.saved[guardUID+"_other"]; !ok {
				t.Error("the other subscription must stay")
			}
		},
	})
}

func TestWriteGuardFinishRegionSelection(t *testing.T) {
	key := guardUID + "_" + alertsub.TypeWeatherWarning
	state := func() *conversation.State {
		return &conversation.State{Type: conversation.TypeWeatherRegionSelect, Regions: []string{"台北市"}}
	}
	runGuard(t, &guardPoint{
		kind: "sub",
		setup: func(e *guardEnv) {
			if err := e.conv.Set(context.Background(), guardUID, state()); err != nil {
				e.t.Fatalf("seed conversation state: %v", err)
			}
		},
		act: func(e *guardEnv, ctx context.Context) { e.h.finishRegionSelection(ctx, guardUID, "tok", state()) },
		present: func(e *guardEnv) bool {
			_, ok := e.subs.saved[key]
			return ok
		},
		ok: "已開啟天氣特報通知", undo: []string{"sub", "conv"},
		canErr: true, errReply: "儲存訂閱失敗",
		undoCallsUnreliable: true,
		afterBetween: func(t *testing.T, e *guardEnv) {
			if convPresent(e) {
				t.Error("the region-selection state must be cleared too")
			}
		},
	})
}

// [F291-07] 問答紀錄：先補刪再回覆.

func TestWriteGuardAnswerHistory(t *testing.T) {
	runGuard(t, &guardPoint{
		kind:    "history",
		act:     func(e *guardEnv, ctx context.Context) { e.h.answer(ctx, guardUID, "晚餐吃什麼比較好", "tok") },
		present: func(e *guardEnv) bool { return len(e.hist.Recent(context.Background(), guardUID)) > 0 },
		ok:      "雞胸肉沙拉", undo: []string{"qa"},
		betweenNot: []string{"雞胸肉沙拉"}, undoNot: []string{erasedMessage, "雞胸肉沙拉"},
		afterBetween: func(t *testing.T, e *guardEnv) {
			if e.hist.repliesAtClear != 0 {
				t.Errorf("replies already sent when Forget ran = %d, want 0 (undo first, reply after)", e.hist.repliesAtClear)
			}
		},
	})
	t.Run("answer_error_skips_recheck", func(t *testing.T) {
		e := newGuardEnv(t)
		e.qa.answer = "" // Answer 回錯 = 沒寫入紀錄
		e.h.erased.mark(context.Background(), guardUID)

		e.h.answer(sentMinuteAgo(), guardUID, "晚餐吃什麼比較好", "tok")

		assertReply(t, e.api.lastReply(t), []string{"目前無法回答"}, []string{erasedMessage, eraseFailedMessage})
		if e.undoCalls["qa"] != 0 {
			t.Errorf("Forget ran %d times, want 0 when Answer failed", e.undoCalls["qa"])
		}
	})
}

// [F291-11] 刪除後再修改不會把餐點建回來：只鎖現況，產品碼不動.

func TestModifyMealAfterErasureDoesNotRecreate(t *testing.T) {
	seed := func(e *guardEnv) {
		now := time.Now()
		if err := e.meals.Meals.Insert(context.Background(), &meal.Meal{
			ID: "m1", UserID: guardUID, EatenAt: now, CreateTime: now, Items: guardFoods, Source: meal.SourceText,
		}); err != nil {
			e.t.Fatalf("seed meal: %v", err)
		}
	}
	change := func(m *meal.Meal) { m.Place = &meal.Place{Name: "林東芳", Source: meal.PlaceFromUser} }
	t.Run("deleted meal is not recreated", func(t *testing.T) {
		e := newGuardEnv(t)
		seed(e)
		if err := e.meals.Meals.DeleteAllByUser(context.Background(), guardUID); err != nil {
			t.Fatalf("erase meals: %v", err)
		}

		got := e.h.modifyMeal(context.Background(), guardUID, "tok", "m1", change)

		if got != nil || len(e.meals.Items) != 0 {
			t.Errorf("modifyMeal = %+v, meals = %+v, want nil and no meals", got, e.meals.Items)
		}
		mustContain(t, e.api.lastReply(t), "找不到")
	})
	t.Run("existing meal is modified", func(t *testing.T) {
		e := newGuardEnv(t)
		seed(e)

		got := e.h.modifyMeal(context.Background(), guardUID, "tok", "m1", change)

		if got == nil || got.Place == nil || got.Place.Name != "林東芳" {
			t.Errorf("modifyMeal = %+v, want the place set", got)
		}
	})
}
