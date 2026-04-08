package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/models"
	"chatbot-go/internal/storage/database/alertsub"
	weatherstore "chatbot-go/internal/storage/database/weather"
	"chatbot-go/internal/weather"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	userstore "chatbot-go/internal/storage/database/user"
)

// GeocodeResult 地理編碼結果.
type GeocodeResult struct {
	City        string
	District    string
	DisplayName string
}

// Geocoder 地理編碼介面（由 geocoding package 實作）.
type Geocoder interface {
	Geocode(ctx context.Context, query string) ([]GeocodeResult, error)
}

// WebhookHandler LINE Webhook 處理器.
type WebhookHandler struct {
	userRepo      userstore.UserRepository
	weatherLookup *weather.Lookup
	alertSubRepo  alertsub.AlertSubRepository
	convManager   *conversation.Manager
	geocoder      Geocoder // 可為 nil（降級：不做 geocoding）
	weatherRepo   weatherstore.WeatherRepository
}

// NewWebhookHandler 建立 Webhook 處理器.
func NewWebhookHandler(
	userRepo userstore.UserRepository,
	weatherLookup *weather.Lookup,
	alertSubRepo alertsub.AlertSubRepository,
	convManager *conversation.Manager,
) *WebhookHandler {
	return &WebhookHandler{
		userRepo:      userRepo,
		weatherLookup: weatherLookup,
		alertSubRepo:  alertSubRepo,
		convManager:   convManager,
	}
}

// NewWebhookHandlerWithOptions 建立 Webhook 處理器（含 geocoder 與 weatherRepo 選項）.
func NewWebhookHandlerWithOptions(
	userRepo userstore.UserRepository,
	weatherLookup *weather.Lookup,
	alertSubRepo alertsub.AlertSubRepository,
	convManager *conversation.Manager,
	geocoder Geocoder,
	weatherRepo weatherstore.WeatherRepository,
) *WebhookHandler {
	return &WebhookHandler{
		userRepo:      userRepo,
		weatherLookup: weatherLookup,
		alertSubRepo:  alertSubRepo,
		convManager:   convManager,
		geocoder:      geocoder,
		weatherRepo:   weatherRepo,
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
		h.handleTextMessage(ctx, event.Source.UserID, event.Message.Text, event.ReplyToken)
	case "location":
		h.handleLocationMessage(ctx, event.Message.Address, event.ReplyToken)
	default:
		if err := ReplyText(ctx, event.ReplyToken, "目前僅支援文字與位置訊息查詢天氣"); err != nil {
			slog.Error("reply unsupported type", "error", err)
		}
	}
}

func (h *WebhookHandler) handleTextMessage(ctx context.Context, userID, text, replyToken string) {
	text = strings.TrimSpace(text)
	text = strings.ReplaceAll(text, "臺", "台")

	// Step 1: 先看對話狀態
	state, err := h.convManager.Get(ctx, userID)
	if err != nil {
		slog.Error("get conversation state", "userID", userID, "error", err)
	}

	if state != nil && state.Type == conversation.TypeLocationClarify {
		h.handleLocationClarify(ctx, userID, text, replyToken, state)
		return
	}

	if h.handleConversation(ctx, userID, text, replyToken, state) {
		return
	}

	// 2. 關鍵字路由
	if h.handleKeyword(ctx, userID, text, replyToken) {
		return
	}

	// Step 2: 精確比對（原有功能）
	forecasts, err := h.weatherLookup.ByDistrict(ctx, text)
	if err != nil {
		slog.Error("weather lookup by district", "district", text, "error", err)
		_ = ReplyText(ctx, replyToken, "查詢天氣資料時發生錯誤，請稍後再試")
		return
	}

	if len(forecasts) > 0 {
		reply := weather.FormatForecasts(forecasts)
		if err := ReplyText(ctx, replyToken, reply); err != nil {
			slog.Error("reply weather text", "error", err)
		}
		return
	}

	// Step 3: Fuzzy 比對
	if h.weatherRepo != nil {
		if done := h.handleFuzzyMatch(ctx, userID, text, replyToken); done {
			return
		}
	}

	// Step 4: Geocoding fallback
	if h.geocoder != nil {
		if done := h.handleGeocodingFallback(ctx, userID, text, replyToken); done {
			return
		}
	}

	_ = ReplyText(ctx, replyToken, "找不到「"+text+"」的天氣資料，請輸入正確的區域名稱")
}

// handleFuzzyMatch 執行模糊比對，回傳 true 表示已處理（不需繼續往下）.
func (h *WebhookHandler) handleFuzzyMatch(ctx context.Context, userID, text, replyToken string) bool {
	candidates, err := h.weatherRepo.FindAllDistricts(ctx)
	if err != nil {
		slog.Error("find all districts", "error", err)
		return false
	}

	matches := weather.FuzzyMatch(candidates, text)

	switch len(matches) {
	case 0:
		return false

	case 1:
		forecast, err := h.weatherLookup.ByCityAndDistrict(ctx, matches[0].City, matches[0].District)
		if err != nil {
			slog.Error("weather lookup by city and district (fuzzy)", "error", err)
			_ = ReplyText(ctx, replyToken, "查詢天氣資料時發生錯誤，請稍後再試")
			return true
		}
		if forecast == nil {
			return false
		}
		reply := weather.FormatSingleForecast(forecast)
		if err := ReplyText(ctx, replyToken, reply); err != nil {
			slog.Error("reply fuzzy weather", "error", err)
		}
		return true

	default:
		// 多個結果 → 存 TypeLocationClarify state，回覆候選清單
		convCandidates := make([]conversation.CandidateLocation, 0, len(matches))
		for _, m := range matches {
			convCandidates = append(convCandidates, conversation.CandidateLocation{
				City:        m.City,
				District:    m.District,
				DisplayName: m.City + m.District,
				Source:      "fuzzy",
			})
		}

		clarifyState := &conversation.State{
			Type:       conversation.TypeLocationClarify,
			Candidates: convCandidates,
		}
		if err := h.convManager.Set(ctx, userID, clarifyState); err != nil {
			slog.Error("set location clarify state", "userID", userID, "error", err)
		}

		_ = ReplyText(ctx, replyToken, buildCandidateMessage(convCandidates))
		return true
	}
}

// handleGeocodingFallback 執行 geocoding 查詢，回傳 true 表示已處理.
func (h *WebhookHandler) handleGeocodingFallback(ctx context.Context, userID, text, replyToken string) bool {
	results, err := h.geocoder.Geocode(ctx, text)
	if err != nil {
		slog.Error("geocode query", "query", text, "error", err)
		return false
	}

	switch len(results) {
	case 0:
		return false

	case 1:
		forecast, err := h.weatherLookup.ByCityAndDistrict(ctx, results[0].City, results[0].District)
		if err != nil {
			slog.Error("weather lookup by city and district (geocode)", "error", err)
			_ = ReplyText(ctx, replyToken, "查詢天氣資料時發生錯誤，請稍後再試")
			return true
		}
		if forecast == nil {
			_ = ReplyText(ctx, replyToken, "抱歉找不到該地點的天氣資料")
			return true
		}
		reply := weather.FormatSingleForecast(forecast) + "\n（資料來源：OpenStreetMap contributors）"
		if err := ReplyText(ctx, replyToken, reply); err != nil {
			slog.Error("reply geocode weather", "error", err)
		}
		return true

	default:
		convCandidates := make([]conversation.CandidateLocation, 0, len(results))
		for _, r := range results {
			displayName := r.DisplayName
			if displayName == "" {
				displayName = r.City + r.District
			}
			convCandidates = append(convCandidates, conversation.CandidateLocation{
				City:        r.City,
				District:    r.District,
				DisplayName: displayName,
				Source:      "geocode",
			})
		}

		clarifyState := &conversation.State{
			Type:       conversation.TypeLocationClarify,
			Candidates: convCandidates,
		}
		if err := h.convManager.Set(ctx, userID, clarifyState); err != nil {
			slog.Error("set location clarify state (geocode)", "userID", userID, "error", err)
		}

		_ = ReplyText(ctx, replyToken, buildCandidateMessage(convCandidates))
		return true
	}
}

// buildCandidateMessage 產生候選地點選擇訊息.
func buildCandidateMessage(candidates []conversation.CandidateLocation) string {
	var sb strings.Builder
	sb.WriteString("找到多個符合的地點，請選擇：\n")
	for i, c := range candidates {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, c.DisplayName)
	}
	sb.WriteString("輸入數字選擇")
	return sb.String()
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
