package server

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/alert"
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/geocoding"
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/liff"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/driver"
	"chatbot-go/internal/platform/health"
	"chatbot-go/internal/platform/middleware"
	"chatbot-go/internal/storage/database"
	"chatbot-go/internal/user"
	"chatbot-go/internal/weather"
	"chatbot-go/internal/webhook"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"golang.org/x/time/rate"
)

// newAIRouter 依設定建立 AI 路由；沒有任何金鑰時回傳 nil.
func newAIRouter(c *config.AIConfig) *ai.Router {
	client := &http.Client{Timeout: 60 * time.Second}
	router := ai.NewRouter(
		&ai.Settings{
			TextOrder:   config.SplitList(c.ProviderOrder),
			VisionOrder: config.SplitList(c.VisionProviderOrder),
		},
		ai.NewGemini(c.GeminiAPIKey, client),
		ai.NewGroq(c.GroqAPIKey, client),
		ai.NewNVIDIA(c.NVIDIAAPIKey, client),
		ai.NewClaude(c.AnthropicAPIKey, c.ClaudeModel),
	)
	if len(router.Configured()) == 0 {
		slog.Warn("no AI provider key set, AI features disabled")
		return nil
	}
	slog.Info("AI providers enabled", "providers", strings.Join(router.Configured(), ","))
	return router
}

// newEcho 建立含逾時、大小上限、安全標頭與請求日誌的 Echo.
func newEcho() *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = httputil.ErrorHandler

	// 連線逾時：預設的 http.Server 沒有任何逾時，慢速連線（Slowloris）可以耗盡資源.
	e.Server.ReadHeaderTimeout = 10 * time.Second
	e.Server.ReadTimeout = 30 * time.Second
	e.Server.WriteTimeout = 30 * time.Second
	e.Server.IdleTimeout = 2 * time.Minute

	// Middleware
	e.Use(echomw.Recover())
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
		Skipper:      func(c echo.Context) bool { return c.Path() == "/health" },
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

// Start 啟動 HTTP server.
func Start(
	repos *database.Repositories,
	weatherScheduler *weather.Scheduler,
	alertScheduler *alert.Scheduler,
	cfg *config.Config,
) error {
	e := newEcho()

	// Health check
	healthHandler := health.NewHealthHandler()
	e.GET("/health", healthHandler.HealthCheck)

	// Weather lookup（webhook + weather handler 共用）
	weatherLookup := weather.NewLookup(
		repos.Weather,
		driver.GetRedisClient(),
		cfg.Weather.CacheTTLSeconds,
	)

	// Conversation state manager
	convManager := conversation.NewManager(driver.GetRedisClient())

	// Geocoding（Nominatim + Redis cache）
	nominatimClient := geocoding.NewNominatimClient()
	geocoder := geocoding.NewCachedGeocoder(nominatimClient, driver.GetRedisClient(), 24*time.Hour)

	// Handlers
	// AI（Gemini / Groq / NVIDIA / Claude 自動選模與故障切換；金鑰都沒設時停用）
	aiRouter := newAIRouter(&cfg.AI)
	var (
		completer weather.Completer
		analyzer  *nutrition.Analyzer
		aiParser  *fitness.AIParser
		helper    *assistant.Assistant
	)
	locations := weather.NewUserLocations(driver.GetRedisClient(), weatherLookup)
	if aiRouter != nil {
		completer = aiRouter
		analyzer = nutrition.NewAnalyzer(aiRouter)
		aiParser = fitness.NewAIParser(aiRouter)
		var weatherContext assistant.WeatherContext
		if locations != nil {
			weatherContext = locations
		}
		helper = assistant.New(&assistant.Config{
			Completer: aiRouter,
			Meals:     repos.Meal,
			Workouts:  repos.Workout,
			Profiles:  repos.Profile,
			Weather:   weatherContext,
			History:   assistant.NewRedisHistory(driver.GetRedisClient()),
			Order:     cfg.AI.QAOrder(),
		})
	}
	advisor := weather.NewAdvisor(completer, driver.GetRedisClient())

	webhookHandler := webhook.NewWebhookHandlerWithOptions(
		repos.User, weatherLookup, repos.AlertSub, convManager, geocoder, repos.Weather,
	).WithAdvisor(advisor).
		WithNutrition(analyzer, repos.Meal, driver.GetRedisClient()).
		WithFitness(repos.Workout, repos.Profile, aiParser).
		WithAssistant(helper, locations)
	if cfg.LIFF.Enabled() {
		webhookHandler.WithLIFF(cfg.LIFF.URL())
	}
	userHandler := user.NewUserHandler(repos.User)
	weatherHandler := weather.NewWeatherHandler(repos.Weather, cfg)
	alertHandler := alert.NewAlertHandler(repos.AlertSub)

	// Routes
	api := e.Group("/api/v1")

	// LINE webhook（帶簽名驗證）
	api.POST("/webhooks", webhookHandler.HandleWebhook, middleware.LineSignatureValidator(driver.GetRedisClient()))

	adminAPI := api.Group("", adminRateLimiter(), middleware.AdminTokenValidator())

	// User
	adminAPI.GET("/users", userHandler.GetAllUsers)

	// Weather（手動觸發同步）
	adminAPI.GET("/openDataUpdate", weatherHandler.TriggerSync)

	// Alert subscriptions
	adminAPI.GET("/alerts/subscriptions/:userID", alertHandler.GetSubscriptions)
	adminAPI.POST("/alerts/subscriptions", alertHandler.Subscribe)
	adminAPI.DELETE("/alerts/subscriptions/:userID/:type", alertHandler.Unsubscribe)

	// LIFF 統計頁（身分由後端驗證 LINE ID token）
	if cfg.LIFF.Enabled() {
		if err := registerLIFF(e, api, repos, cfg); err != nil {
			return err
		}
	} else {
		slog.Info("LIFF not configured, stats page disabled")
	}

	// Graceful shutdown
	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	go func() {
		slog.Info("server started", "port", cfg.Server.Port)
		if err := e.Start(addr); err != nil {
			slog.Info("server shutdown", "reason", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down server...")

	// 停止排程器
	if weatherScheduler != nil {
		if err := weatherScheduler.Stop(); err != nil {
			slog.Error("stop weather scheduler", "error", err)
		}
	}
	if alertScheduler != nil {
		if err := alertScheduler.Stop(); err != nil {
			slog.Error("stop alert scheduler", "error", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := e.Shutdown(ctx); err != nil {
		slog.Error("server forced shutdown", "error", err)
		return err
	}

	slog.Info("server stopped gracefully")
	return nil
}

// registerLIFF 註冊統計頁與 /api/v1/me 資料 API.
func registerLIFF(e *echo.Echo, api *echo.Group, repos *database.Repositories, cfg *config.Config) error {
	page, static, err := liff.Page(cfg.LIFF.ID)
	if err != nil {
		return fmt.Errorf("load LIFF page: %w", err)
	}
	e.GET("/liff", page)
	e.GET("/liff/", page)
	e.GET("/liff/static/*", static)

	verifier := liff.NewVerifier(cfg.LIFF.ChannelID, &http.Client{Timeout: 10 * time.Second}, driver.GetRedisClient())
	h := liff.NewHandler(repos.Meal, repos.Workout, repos.Profile)
	me := api.Group("/me", perIPRateLimiter(5, 30), liff.NoStore, liff.Auth(verifier))
	me.GET("/summary", h.Summary)
	me.GET("/day", h.Day)
	me.GET("/profile", h.GetProfile)
	me.PUT("/profile", h.PutProfile)
	me.DELETE("/meals/:id", h.DeleteMeal)
	me.DELETE("/workouts/:id", h.DeleteWorkout)
	slog.Info("LIFF stats page enabled")
	return nil
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
