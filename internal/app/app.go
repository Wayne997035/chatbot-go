// Package app 組裝層：建立所有依賴（資料庫、LINE、AI、各功能的處理器與排程），啟動服務，關機時依序收尾.
// 其他套件不讀全域設定、不自己建立連線，需要什麼都由這裡傳入.
package app

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/alert"
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/geocoding"
	"chatbot-go/internal/liff"
	"chatbot-go/internal/line"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/platform/config"
	"chatbot-go/internal/platform/driver"
	"chatbot-go/internal/platform/health"
	"chatbot-go/internal/platform/server"
	"chatbot-go/internal/storage/database"
	"chatbot-go/internal/user"
	"chatbot-go/internal/weather"
	"chatbot-go/internal/webhook"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Run 連線資料庫、組裝服務並執行，直到收到關機訊號；回傳前會關閉所有連線.
func Run(ctx context.Context, cfg *config.Config) error {
	mongoClient, db, err := driver.ConnectMongo(ctx, &cfg.Database.Mongo)
	if err != nil {
		return err
	}
	defer driver.CloseMongo(ctx, mongoClient)

	// Redis 連不上不影響啟動，降級為無快取（各元件都接受 nil）.
	redisClient, err := driver.ConnectRedis(ctx, &cfg.Database.Redis)
	if err != nil {
		slog.Warn("Redis unavailable, running without cache", "error", err)
		redisClient = nil
	}
	defer driver.CloseRedis(redisClient)

	a, err := build(cfg, db, redisClient) //nolint:contextcheck // 路由的 middleware 用的是每個請求自己的 context
	if err != nil {
		return err
	}
	a.startSchedulers(cfg)
	return server.Run(ctx, a.echo, fmt.Sprintf(":%d", cfg.Server.Port), a.stoppers())
}

// app 組裝好的服務.
type app struct {
	echo             *echo.Echo
	webhook          *webhook.WebhookHandler
	weatherHandler   *weather.WeatherHandler
	weatherScheduler *weather.Scheduler
	alertScheduler   *alert.Scheduler
}

// build 建立所有元件並註冊路由；不啟動任何背景工作.
func build(cfg *config.Config, db *mongo.Database, redisClient *redis.Client) (*app, error) {
	repos := database.NewRepositories(db)
	lineClient := line.NewClient(lineConfig(&cfg.Line), nil)

	// 天氣查詢（webhook 與天氣 API 共用）
	weatherLookup := weather.NewLookup(repos.Weather, redisClient, cfg.Weather.CacheTTLSeconds)
	convManager := conversation.NewManager(redisClient)
	// 地名查詢（Nominatim + Redis 快取）
	geocoder := geocoding.NewCachedGeocoder(geocoding.NewNominatimClient(), redisClient, 24*time.Hour)

	// AI（Gemini / Groq / NVIDIA / Claude 自動選模與故障切換；金鑰都沒設時停用）
	aiRouter := newAIRouter(&cfg.AI)
	var (
		completer weather.Completer
		analyzer  *nutrition.Analyzer
		aiParser  *fitness.AIParser
		helper    *assistant.Assistant
	)
	locations := weather.NewUserLocations(redisClient, weatherLookup)
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
			Alerts:    repos.AlertSub,
			History:   assistant.NewRedisHistory(redisClient),
			Order:     cfg.AI.QAOrder(),
		})
	}

	webhookHandler := webhook.NewWebhookHandlerWithOptions(
		lineClient, repos.User, weatherLookup, repos.AlertSub, convManager, geocoder, repos.Weather,
	).WithAdvisor(weather.NewAdvisor(completer, redisClient)).
		WithNutrition(analyzer, repos.Meal, redisClient).
		WithFitness(repos.Workout, repos.Profile, aiParser).
		WithAssistant(helper, locations)
	if cfg.LIFF.Enabled() {
		webhookHandler.WithLIFF(cfg.LIFF.URL())
	}

	weatherScheduler, err := weather.NewScheduler(repos.Weather, cfg)
	if err != nil {
		return nil, err
	}
	alertChecker := alert.NewChecker(repos.AlertSub, lineClient, redisClient, cfg)
	// [F241-11][F241-12] 刪除資料時清警報推播紀錄；推播前查刪除標記（標記由 webhook 擁有，這裡借用）.
	webhookHandler.WithAlertHistory(alertChecker)
	alertChecker.WithErasureCheck(webhookHandler)
	alertScheduler, err := alert.NewScheduler(alertChecker, cfg)
	if err != nil {
		return nil, err
	}

	handlers := &server.Handlers{
		Health:  health.NewHealthHandler(dependencyChecks(db, redisClient)...).WithKeepAlive(redisKeepAlive(redisClient), time.Now),
		Webhook: webhookHandler,
		User:    user.NewUserHandler(repos.User),
		Weather: weather.NewWeatherHandler(repos.Weather, cfg),
		Alert:   alert.NewAlertHandler(repos.AlertSub),
	}
	if cfg.LIFF.Enabled() {
		page, static, err := liff.Page(cfg.LIFF.ID)
		if err != nil {
			return nil, fmt.Errorf("load LIFF page: %w", err)
		}
		handlers.LIFF = &server.LIFF{
			Page:     page,
			Static:   static,
			API:      liff.NewHandler(repos.Meal, repos.Workout, repos.Profile).WithErasureCheck(webhookHandler),
			Verifier: liff.NewVerifier(cfg.LIFF.ChannelID, &http.Client{Timeout: 10 * time.Second}, redisClient),
		}
		slog.Info("LIFF stats page enabled")
	} else {
		slog.Info("LIFF not configured, stats page disabled")
	}

	e, err := server.New(cfg.Server.TrustedProxies)
	if err != nil {
		return nil, err
	}
	server.Routes(e, handlers, server.Security{
		ChannelSecret: cfg.Line.ChannelSecret,
		AdminToken:    cfg.Security.AdminToken,
		Replay:        redisClient,
	})

	return &app{
		echo:             e,
		webhook:          webhookHandler,
		weatherHandler:   handlers.Weather,
		weatherScheduler: weatherScheduler,
		alertScheduler:   alertScheduler,
	}, nil
}

// startSchedulers 有氣象署金鑰才啟動天氣同步與警報檢查.
func (a *app) startSchedulers(cfg *config.Config) {
	if cfg.Weather.CWAAuthKey == "" {
		slog.Warn("CWA auth key not set, weather scheduler and alerts disabled")
		return
	}
	if err := a.weatherScheduler.Start(); err != nil {
		slog.Error("weather scheduler start failed", "error", err)
	}
	if cfg.Alert.Cron == "" {
		slog.Warn("alert cron not set, alert scheduler disabled")
		return
	}
	if err := a.alertScheduler.Start(); err != nil {
		slog.Error("alert scheduler start failed", "error", err)
	}
}

// stoppers 關機時要停止的背景工作（同時進行）.
func (a *app) stoppers() []server.Stopper {
	return []server.Stopper{
		{Name: "webhook events", Stop: a.webhook.Shutdown},
		{Name: "manual weather sync", Stop: a.weatherHandler.Stop},
		{Name: "alert checks", Stop: a.alertScheduler.Stop},
		{Name: "weather sync", Stop: a.weatherScheduler.Stop},
	}
}

// lineConfig 把設定檔的 LINE 區段轉成 LINE 用戶端的設定.
func lineConfig(c *config.LineConfig) *line.Config {
	return &line.Config{
		Token:      c.ChannelToken,
		ReplyURL:   c.ReplyURL,
		PushURL:    c.PushURL,
		ContentURL: c.ContentURL,
		LoadingURL: c.LoadingURL,
	}
}

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

// dependencyChecks /ready 要檢查的依賴：MongoDB 必要，Redis 非必要（沒有它只是功能變少）.
func dependencyChecks(db *mongo.Database, redisClient *redis.Client) []health.Check {
	return []health.Check{
		{Name: "mongo", Required: true, Ping: func(ctx context.Context) error {
			return db.Client().Ping(ctx, nil)
		}},
		{Name: "redis", Ping: func(ctx context.Context) error {
			if redisClient == nil {
				return errors.New("redis not connected")
			}
			return redisClient.Ping(ctx).Err()
		}},
	}
}

// redisKeepAliveKey [F24-81] 保活寫入的固定 key；值只有時間，不含任何請求資料.
const redisKeepAliveKey = "keepalive:health"

// redisKeepAliveTTL [F24-81] 保活 key 的存活時間.
const redisKeepAliveTTL = time.Hour

// redisKeepAlive [F24-81] 回傳 /health 觸發的保活寫入函式，讓免費 Redis 不因閒置被刪；Redis 沒連上回 nil（不啟用）.
func redisKeepAlive(redisClient *redis.Client) func(ctx context.Context) error {
	if redisClient == nil {
		return nil
	}
	return func(ctx context.Context) error {
		return redisClient.Set(ctx, redisKeepAliveKey, time.Now().UTC().Format(time.RFC3339), redisKeepAliveTTL).Err()
	}
}
