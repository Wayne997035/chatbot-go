package server

import (
	"chatbot-go/internal/alert"
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/liff"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/health"
	"chatbot-go/internal/platform/middleware"
	"chatbot-go/internal/user"
	"chatbot-go/internal/weather"
	"chatbot-go/internal/webhook"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// logPanic [F24-94] 把 recover 到的 panic 寫進 slog；path 是路由樣式，不是請求網址.
func logPanic(c echo.Context, err error, stack []byte) error {
	slog.ErrorContext(c.Request().Context(), "panic recovered", "error", err, "path", c.Path(), "stack", string(stack))
	return err
}

// newEcho 建立含逾時、大小上限、安全標頭與請求日誌的 Echo.
func newEcho() *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = httputil.ErrorHandler
	// [F24-94] 必須設 e.StdLogger：echo 在 Start 時會把 Server.ErrorLog 蓋成 e.StdLogger.
	e.StdLogger = slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)
	e.Server.ErrorLog = e.StdLogger
	// [F24-94] 不印 echo 的位址橫幅（直寫 stdout，繞過 slog）.
	e.HidePort = true

	// 連線逾時：預設的 http.Server 沒有任何逾時，慢速連線（Slowloris）可以耗盡資源.
	e.Server.ReadHeaderTimeout = 10 * time.Second
	e.Server.ReadTimeout = 30 * time.Second
	e.Server.WriteTimeout = 30 * time.Second
	e.Server.IdleTimeout = 2 * time.Minute

	// Middleware
	// [F24-94] panic 與堆疊走 slog（經 log 出口遮蔽），不讓 echo 自己寫 stderr.
	e.Use(echomw.RecoverWithConfig(echomw.RecoverConfig{LogErrorFunc: logPanic}))
	// 全站請求大小上限（LINE webhook 另有 1 MB 檢查）.
	e.Use(echomw.BodyLimit("2M"))
	// 安全標頭：nosniff、禁止被其他網站框住、HTTPS 下啟用 HSTS.
	e.Use(echomw.SecureWithConfig(echomw.SecureConfig{
		XSSProtection:      "0",
		ContentTypeNosniff: "nosniff",
		XFrameOptions:      "DENY",
		HSTSMaxAge:         31536000,
		ReferrerPolicy:     "no-referrer",
	}))
	// 請求日誌：記路由樣式（如 /alerts/subscriptions/:userID）而非實際 URL，避免把使用者 ID 寫進 log.
	e.Use(echomw.RequestLoggerWithConfig(echomw.RequestLoggerConfig{
		Skipper: func(c echo.Context) bool { return c.Path() == "/health" || c.Path() == "/ready" },
		// HandleError：先讓錯誤處理器寫出回應，記到的才是真正的狀態碼（以前錯誤一律記成 200）.
		HandleError:  true,
		LogStatus:    true,
		LogLatency:   true,
		LogMethod:    true,
		LogRoutePath: true,
		LogValuesFunc: func(_ echo.Context, v echomw.RequestLoggerValues) error {
			slog.Info("request", "method", v.Method, "route", v.RoutePath, "status", v.Status, "latency", v.Latency)
			return nil
		},
	}))
	return e
}

// New 建立 Echo（逾時、大小上限、安全標頭、請求日誌）並設定限流用的來源 IP 判斷方式.
func New(trustedProxies string) (*echo.Echo, error) {
	e := newEcho()
	extractor, err := ipExtractor(trustedProxies)
	if err != nil {
		return nil, err
	}
	e.IPExtractor = extractor
	return e, nil
}

// Handlers 路由用的處理器；LIFF 為 nil 表示沒有啟用統計頁.
type Handlers struct {
	Health  *health.HealthHandler
	Webhook *webhook.WebhookHandler
	User    *user.UserHandler
	Weather *weather.WeatherHandler
	Alert   *alert.AlertHandler
	LIFF    *LIFF
}

// LIFF 統計頁：頁面、靜態檔與 /api/v1/me 資料 API（身分由 Verifier 驗證 LINE ID token）.
type LIFF struct {
	Page     echo.HandlerFunc
	Static   echo.HandlerFunc
	API      *liff.Handler
	Verifier *liff.Verifier
}

// Security 路由的驗證設定.
type Security struct {
	ChannelSecret string        // 驗證 LINE webhook 簽章
	AdminToken    string        // 管理 API；空字串時管理 API 一律回 503
	Replay        *redis.Client // webhook 防重放紀錄；nil 時只用記憶體
}

// Routes 註冊所有路由.
func Routes(e *echo.Echo, h *Handlers, sec Security) {
	// /health 只代表程式活著；/ready 檢查 MongoDB（必要）與 Redis（非必要）
	e.GET("/health", h.Health.HealthCheck)
	e.GET("/ready", h.Health.Ready)

	api := e.Group("/api/v1")
	api.POST("/webhooks", h.Webhook.HandleWebhook, middleware.LineSignatureValidator(sec.ChannelSecret, sec.Replay))

	admin := api.Group("", adminRateLimiter(), middleware.AdminTokenValidator(sec.AdminToken))
	admin.GET("/users", h.User.GetAllUsers)
	admin.GET("/openDataUpdate", h.Weather.TriggerSync)
	admin.GET("/alerts/subscriptions/:userID", h.Alert.GetSubscriptions)
	admin.POST("/alerts/subscriptions", h.Alert.Subscribe)
	admin.DELETE("/alerts/subscriptions/:userID/:type", h.Alert.Unsubscribe)

	if h.LIFF != nil {
		e.GET("/liff", h.LIFF.Page)
		e.GET("/liff/", h.LIFF.Page)
		e.GET("/liff/static/*", h.LIFF.Static)
		h.LIFF.API.Routes(api.Group("/me", perIPRateLimiter(5, 30), liff.NoStore, liff.Auth(h.LIFF.Verifier)))
	}
}

// Run 啟動 HTTP 伺服器，直到啟動失敗、收到 SIGINT / SIGTERM 或 ctx 取消，然後依序關機（見 shutdown）.
func Run(ctx context.Context, e *echo.Echo, addr string, stoppers []Stopper) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := serve(e, addr, ctx.Done())
	if serveErr != nil {
		slog.Error("http server failed", "error", serveErr)
	} else {
		slog.Info("shutting down server...")
	}
	if err := errors.Join(serveErr, shutdown(ctx, e, stoppers)); err != nil {
		return err
	}
	slog.Info("server stopped gracefully")
	return nil
}

// shutdownTimeout 關機時最多等多久：照片分析最多約 40 秒，讓處理中的訊息能回覆完.
// 部署平台給的關機寬限要更長（docker compose 的 stop_grace_period、Kubernetes 的 terminationGracePeriodSeconds 設 60 秒）.
const shutdownTimeout = 45 * time.Second

// Stopper 關機時要停止的背景工作.
type Stopper struct {
	Name string
	Stop func(ctx context.Context) error
}

// shutdown 先停止接收新的 HTTP 請求，再同時等所有背景工作做完（逾時就取消）.
// 回傳後才關閉資料庫連線（main 的 defer），處理中的工作不會寫到一半失去連線.
// ctx 通常已經因為關機訊號而取消，所以關機的期限另外從 context.WithoutCancel(ctx) 算.
func shutdown(ctx context.Context, e *echo.Echo, stoppers []Stopper) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	var errs []error
	if err := e.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("http server: %w", err))
	}
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for _, s := range stoppers {
		wg.Go(func() {
			if err := s.Stop(ctx); err != nil {
				slog.Error("stop background work", "name", s.Name, "error", err)
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", s.Name, err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// ipExtractor 決定限流用的來源 IP. 沒設定可信任代理時用連線位址（不相信任何 header，偽造無效）；
// 放在 HTTPS 代理後面時要設定代理的位址範圍，否則所有人看起來都是代理的 IP，限流會變成全體共用.
func ipExtractor(trustedProxies string) (echo.IPExtractor, error) {
	cidrs := config.SplitList(trustedProxies)
	if len(cidrs) == 0 {
		return echo.ExtractIPDirect(), nil
	}
	opts := []echo.TrustOption{echo.TrustLoopback(false), echo.TrustLinkLocal(false), echo.TrustPrivateNet(false)}
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid TRUSTED_PROXIES entry %q: %w", cidr, err)
		}
		opts = append(opts, echo.TrustIPRange(ipNet))
	}
	return echo.ExtractIPFromXFFHeader(opts...), nil
}

// serve 啟動 HTTP 伺服器並等到收到關機訊號. 啟動失敗（例如 port 被佔用）時立刻回傳錯誤，
// 讓程式結束、交給容器的重啟機制，而不是活著卻沒在服務.
func serve(e *echo.Echo, addr string, stop <-chan struct{}) error {
	errCh := make(chan error, 1)
	go func() { errCh <- e.Start(addr) }()
	slog.Info("server starting", "addr", addr)
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http server: %w", err)
	case <-stop:
		return nil
	}
}

// perIPRateLimiter 依來源 IP 限流.
func perIPRateLimiter(perSecond float64, burst int) echo.MiddlewareFunc {
	return echomw.RateLimiterWithConfig(echomw.RateLimiterConfig{
		Store: echomw.NewRateLimiterMemoryStoreWithConfig(echomw.RateLimiterMemoryStoreConfig{
			Rate: rate.Limit(perSecond), Burst: burst, ExpiresIn: 3 * time.Minute,
		}),
		IdentifierExtractor: func(c echo.Context) (string, error) { return c.RealIP(), nil },
		DenyHandler: func(c echo.Context, _ string, _ error) error {
			return c.JSON(http.StatusTooManyRequests, httputil.ErrorWithCode(httputil.ErrorCodeInvalidParameter, "Too many requests"))
		},
	})
}

func adminRateLimiter() echo.MiddlewareFunc {
	return echomw.RateLimiterWithConfig(echomw.RateLimiterConfig{
		Store: echomw.NewRateLimiterMemoryStoreWithConfig(echomw.RateLimiterMemoryStoreConfig{
			Rate:      rate.Limit(1),
			Burst:     10,
			ExpiresIn: 3 * time.Minute,
		}),
		IdentifierExtractor: func(c echo.Context) (string, error) {
			return c.RealIP(), nil
		},
		DenyHandler: func(c echo.Context, _ string, _ error) error {
			return c.JSON(http.StatusTooManyRequests, httputil.ErrorWithCode(
				httputil.ErrorCodeInvalidParameter, "Too many admin requests"))
		},
	})
}
