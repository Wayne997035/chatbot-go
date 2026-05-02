package server

import (
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
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"golang.org/x/time/rate"
)

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
	webhookHandler := webhook.NewWebhookHandlerWithOptions(
		repos.User, weatherLookup, repos.AlertSub, convManager, geocoder, repos.Weather,
	)
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
