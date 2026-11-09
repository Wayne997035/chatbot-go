package health

import (
	"chatbot-go/internal/redact"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"
)

// readyTimeout 每次 readiness 檢查的時間上限.
const readyTimeout = 2 * time.Second

// keepAliveInterval [F24-81] Redis 保活寫入的最短間隔；遠小於免費 Redis 的 14 天閒置門檻.
const keepAliveInterval = 10 * time.Minute

// readyCacheTTL /ready 結果的快取時間：公開端點被狂打時，後端 ping 每 5 秒最多一輪.
const readyCacheTTL = 5 * time.Second

// statusOK 正常時的狀態.
const statusOK = "ok"

// Check 一項依賴的檢查.
type Check struct {
	Name string
	// Required 必要依賴失敗時 /ready 回 503；非必要的只標記 degraded（例如 Redis：沒有它也能服務，只是功能變少）.
	Required bool
	Ping     func(ctx context.Context) error
}

// HealthHandler 健康檢查處理器.
type HealthHandler struct {
	checks []Check
	now    func() time.Time

	// [F24-81] 保活：keepAlive 為 nil 表示未啟用；lastKeepAlive 是上次嘗試的 UnixNano（0 = 從未）.
	keepAlive     func(ctx context.Context) error
	lastKeepAlive atomic.Int64
	keepAliveWG   sync.WaitGroup

	// /ready 結果快取；readyMu 在 ping 期間也持有，並行請求只會 ping 一輪.
	readyMu   sync.Mutex
	readyLast *readyResult
}

// readyResult 上一次 /ready 的結果.
type readyResult struct {
	code int
	body map[string]any
	at   time.Time
}

// NewHealthHandler 建立健康檢查處理器；checks 用於 /ready.
func NewHealthHandler(checks ...Check) *HealthHandler {
	return &HealthHandler{checks: checks, now: time.Now}
}

// WithKeepAlive [F24-81] 啟用 Redis 保活：/health 被呼叫時，距上次嘗試滿 keepAliveInterval 才呼叫 write.
// write 為 nil 不啟用（例如 Redis 沒連上）；now 為 nil 用 time.Now.
func (h *HealthHandler) WithKeepAlive(write func(ctx context.Context) error, now func() time.Time) *HealthHandler {
	if write == nil {
		return h
	}
	if now != nil {
		h.now = now
	}
	h.keepAlive = write
	return h
}

// maybeKeepAlive [F24-81] 每 keepAliveInterval 最多寫一次. 嘗試時間在寫入前更新，失敗不回滾，
// 避免 Redis 壞掉時外人狂打 /health 就每次都打一次 Redis. 寫入在背景進行，/health 不等它；
// 時鐘倒退（差值為負）視為到期，不讓保活停到時鐘追上.
func (h *HealthHandler) maybeKeepAlive(ctx context.Context) {
	last := h.lastKeepAlive.Load()
	t := h.now()
	if d := t.Sub(time.Unix(0, last)); last != 0 && d >= 0 && d < keepAliveInterval {
		return
	}
	if !h.lastKeepAlive.CompareAndSwap(last, t.UnixNano()) {
		return // 別的請求搶到了
	}
	h.keepAliveWG.Add(1)
	go h.writeKeepAlive(context.WithoutCancel(ctx))
}

// writeKeepAlive 在背景寫入；脫離請求 context，客戶端斷線不會取消它. panic 只記 log，不讓程序結束.
func (h *HealthHandler) writeKeepAlive(ctx context.Context) {
	defer h.keepAliveWG.Done()
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("Redis keep-alive write panicked; /health still reports ok", "panic", redact.String(fmt.Sprint(r)))
		}
	}()
	wctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := h.keepAlive(wctx); err != nil {
		slog.Warn("Redis keep-alive write failed; /health still reports ok", "error", redact.Error(err))
	}
}

// waitKeepAlive 等背景保活寫入結束；只給同套件測試用，呼叫前發出請求的 goroutine 都要已結束.
func (h *HealthHandler) waitKeepAlive() {
	h.keepAliveWG.Wait()
}

// HealthCheck GET /health：程式還活著（liveness）. 不檢查依賴，資料庫掛了重啟程式也沒用.
// 啟用保活時順便觸發 Redis 寫入，失敗不影響回應.
func (h *HealthHandler) HealthCheck(c echo.Context) error {
	if h.keepAlive != nil {
		h.maybeKeepAlive(c.Request().Context())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": statusOK})
}

// Ready GET /ready：依賴是否可用（readiness）. 必要依賴失敗回 503. 回應只有狀態，錯誤細節只寫進 log.
// 結果快取 readyCacheTTL；ping 期間持鎖，並行請求等同一輪結果. 時間在取鎖後才讀：排隊的請求若先讀，
// 手上的時間會早於前一輪存下的時間而被當成過期，又各自重 ping 一次. ping 不跟請求 context 取消，
// 第一個請求的客戶端斷線不會把「失敗」快取給所有人.
func (h *HealthHandler) Ready(c echo.Context) error {
	h.readyMu.Lock()
	defer h.readyMu.Unlock()
	if r := h.readyLast; r != nil {
		if d := h.now().Sub(r.at); d >= 0 && d < readyCacheTTL {
			return c.JSON(r.code, r.body)
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request().Context()), readyTimeout)
	defer cancel()
	status, code := statusOK, http.StatusOK
	results := make(map[string]string, len(h.checks))
	for _, check := range h.checks {
		if err := check.Ping(ctx); err != nil {
			slog.Warn("readiness check failed", "check", check.Name, "error", redact.Error(err))
			if check.Required {
				results[check.Name] = "fail"
				status, code = "unavailable", http.StatusServiceUnavailable
				continue
			}
			results[check.Name] = "degraded"
			if status == statusOK {
				status = "degraded"
			}
			continue
		}
		results[check.Name] = statusOK
	}
	body := map[string]any{"status": status, "checks": results}
	h.readyLast = &readyResult{code: code, body: body, at: h.now()}
	return c.JSON(code, body)
}
