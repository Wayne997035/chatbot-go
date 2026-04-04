package webhook

import (
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/models"
	"chatbot-go/internal/weather"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	userstore "chatbot-go/internal/storage/database/user"
)

// WebhookHandler LINE Webhook 處理器.
type WebhookHandler struct {
	userRepo      userstore.UserRepository
	weatherLookup *weather.Lookup
}

// NewWebhookHandler 建立 Webhook 處理器.
func NewWebhookHandler(userRepo userstore.UserRepository, weatherLookup *weather.Lookup) *WebhookHandler {
	return &WebhookHandler{
		userRepo:      userRepo,
		weatherLookup: weatherLookup,
	}
}

// HandleWebhook 接收 LINE Webhook 事件.
func (h *WebhookHandler) HandleWebhook(c echo.Context) error {
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, httputil.ErrorWithCode(
			httputil.ErrorCodeInvalidParameter, "Failed to read request body"))
	}

	// 非同步處理，立即回覆 200（LINE 要求 3 秒內回覆）
	go h.processEvents(body)

	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

func (h *WebhookHandler) processEvents(body []byte) {
	var wrapper models.EventWrapper
	if err := json.Unmarshal(body, &wrapper); err != nil {
		slog.Error("parse webhook events", "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i := range wrapper.Events {
		event := &wrapper.Events[i]
		if event.Type != "message" {
			continue
		}

		// 儲存使用者（非同步，不阻塞回覆）
		go h.saveUser(ctx, event.Source.UserID)

		h.handleMessage(ctx, event)
	}
}

func (h *WebhookHandler) handleMessage(ctx context.Context, event *models.Event) {
	switch event.Message.Type {
	case "text":
		h.handleTextMessage(ctx, event.Message.Text, event.ReplyToken)
	case "location":
		h.handleLocationMessage(ctx, event.Message.Address, event.ReplyToken)
	default:
		if err := ReplyText(ctx, event.ReplyToken, "目前僅支援文字與位置訊息查詢天氣"); err != nil {
			slog.Error("reply unsupported type", "error", err)
		}
	}
}

func (h *WebhookHandler) handleTextMessage(ctx context.Context, text, replyToken string) {
	text = strings.TrimSpace(text)
	text = strings.ReplaceAll(text, "臺", "台")

	forecasts, err := h.weatherLookup.ByDistrict(ctx, text)
	if err != nil {
		slog.Error("weather lookup by district", "district", text, "error", err)
		_ = ReplyText(ctx, replyToken, "查詢天氣資料時發生錯誤，請稍後再試")
		return
	}

	if len(forecasts) == 0 {
		_ = ReplyText(ctx, replyToken, "找不到「"+text+"」的天氣資料，請輸入正確的區域名稱")
		return
	}

	reply := weather.FormatForecasts(forecasts)
	if err := ReplyText(ctx, replyToken, reply); err != nil {
		slog.Error("reply weather text", "error", err)
	}
}

func (h *WebhookHandler) handleLocationMessage(ctx context.Context, address, replyToken string) {
	city, district := ParseAddress(address)
	if city == "" || district == "" {
		_ = ReplyText(ctx, replyToken, "無法解析地址，請傳送正確的位置資訊")
		return
	}

	forecast, err := h.weatherLookup.ByCityAndDistrict(ctx, city, district)
	if err != nil {
		slog.Error("weather lookup by location", "city", city, "district", district, "error", err)
		_ = ReplyText(ctx, replyToken, "查詢天氣資料時發生錯誤，請稍後再試")
		return
	}

	if forecast == nil {
		_ = ReplyText(ctx, replyToken, "找不到「"+city+district+"」的天氣資料")
		return
	}

	reply := weather.FormatSingleForecast(forecast)
	if err := ReplyText(ctx, replyToken, reply); err != nil {
		slog.Error("reply weather location", "error", err)
	}
}

func (h *WebhookHandler) saveUser(ctx context.Context, userID string) {
	if userID == "" {
		return
	}
	user := &userstore.User{
		ID:         userID,
		CreateTime: time.Now(),
	}
	if err := h.userRepo.Upsert(ctx, user); err != nil {
		slog.Error("save user", "userID", userID, "error", err)
	}
}
