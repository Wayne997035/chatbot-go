package health

import (
	"chatbot-go/internal/redact"
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"
)

// readyTimeout 每次 readiness 檢查的時間上限.
const readyTimeout = 2 * time.Second

// keepAliveInterval [F24-81] Redis 保活寫入的最短間隔；遠小於免費 Redis 的 14 天閒置門檻.
const keepAliveInterval = 10 * time.Minute

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

	// [F24-81] 保活：keepAlive 為 nil 表示未啟用；lastKeepAlive 是上次嘗試的 UnixNano（0 = 從未）.
	keepAlive     func(ctx context.Context) error
	now           func() time.Time
	lastKeepAlive atomic.Int64
}

// NewHealthHandler 建立健康檢查處理器；checks 用於 /ready.
func NewHealthHandler(checks ...Check) *HealthHandler {
	return &HealthHandler{checks: checks}
}

// WithKeepAlive [F24-81] 啟用 Redis 保活：/health 被呼叫時，距上次嘗試滿 keepAliveInterval 才呼叫 write.
// write 為 nil 不啟用（例如 Redis 沒連上）；now 為 nil 用 time.Now.
func (h *HealthHandler) WithKeepAlive(write func(ctx context.Context) error, now func() time.Time) *HealthHandler {
	if write == nil {
		return h
	}
	if now == nil {
		now = time.Now
	}
	h.keepAlive, h.now = write, now
	return h
}

// maybeKeepAlive [F24-81] 每 keepAliveInterval 最多寫一次. 嘗試時間在寫入前更新，失敗不回滾，
// 避免 Redis 壞掉時外人狂打 /health 就每次都打一次 Redis. 寫入脫離請求 context，客戶端斷線不會取消它.
func (h *HealthHandler) maybeKeepAlive(ctx context.Context) {
	t := h.now()
	last := h.lastKeepAlive.Load()
	if last != 0 && t.Sub(time.Unix(0, last)) < keepAliveInterval {
		return
	}
	if !h.lastKeepAlive.CompareAndSwap(last, t.UnixNano()) {
		return // 別的請求搶到了
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readyTimeout)
	defer cancel()
	if err := h.keepAlive(wctx); err != nil {
		slog.Warn("Redis keep-alive write failed; /health still reports ok", "error", redact.Error(err))
	}
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
func (h *HealthHandler) Ready(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), readyTimeout)
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
	return c.JSON(code, map[string]any{"status": status, "checks": results})
}
