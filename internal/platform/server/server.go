package server

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/alert"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/geocoding"
	"chatbot-go/internal/httputil"
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
	)
	if len(router.Configured()) == 0 {
		slog.Warn("no AI provider key set, AI features disabled")
		return nil
	}
	slog.Info("AI providers enabled", "providers", strings.Join(router.Configured(), ","))
	return router
}

// Start 啟動 HTTP server.
func Start(
	repos *database.Repositories,
	weatherScheduler *weather.Scheduler,
	alertScheduler *alert.Scheduler,
	cfg *config.Config,
) error {
	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = httputil.ErrorHandler

	// Middleware
	e.Use(echomw.Recover())
	e.Use(echomw.LoggerWithConfig(echomw.LoggerConfig{
		Skipper: func(c echo.Context) bool {
			return c.Path() == "/health"
		},
	}))

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
	// AI（Gemini / Groq / NVIDIA 自動選模與故障切換；金鑰都沒設時停用）
	aiRouter := newAIRouter(&cfg.AI)
	var completer weather.Completer
	if aiRouter != nil {
		completer = aiRouter
	}
	advisor := weather.NewAdvisor(completer, driver.GetRedisClient())

	webhookHandler := webhook.NewWebhookHandlerWithOptions(
		repos.User, weatherLookup, repos.AlertSub, convManager, geocoder, repos.Weather,
	).WithAdvisor(advisor)
	userHandler := user.NewUserHandler(repos.User)
	weatherHandler := weather.NewWeatherHandler(repos.Weather, cfg)
	alertHandler := alert.NewAlertHandler(repos.AlertSub)

	// Routes
	api := e.Group("/api/v1")

	// LINE webhook（帶簽名驗證）
	api.POST("/webhooks", webhookHandler.HandleWebhook, middleware.LineSignatureValidator())

	// User
	api.GET("/users", userHandler.GetAllUsers)

	// Weather（手動觸發同步）
	api.GET("/openDataUpdate", weatherHandler.TriggerSync)

	// Alert subscriptions
	api.GET("/alerts/subscriptions/:userID", alertHandler.GetSubscriptions)
	api.POST("/alerts/subscriptions", alertHandler.Subscribe)
	api.DELETE("/alerts/subscriptions/:userID/:type", alertHandler.Unsubscribe)

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
