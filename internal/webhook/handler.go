package webhook

import (
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/fitness"
	"chatbot-go/internal/geocoding"
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/line"
	"chatbot-go/internal/models"
	"chatbot-go/internal/nutrition"
	"chatbot-go/internal/platform/background"
	"chatbot-go/internal/platform/localcache"
	"chatbot-go/internal/recentmeal"
	"chatbot-go/internal/redact"
	"chatbot-go/internal/storage/database/meal"
	userstore "chatbot-go/internal/storage/database/user"
	"chatbot-go/internal/weather"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

const maxWebhookBodyBytes int64 = 1 << 20

// dailyLimitWarnAbove 每日計數筆數超過這個值才發 Warn（不拒收）.
// 算式：使用者 U=500 × 次數種類 3（photo、qa:count、mealtext）× 同時存活的日期 key 2（TTL 26 小時，跨兩個台灣日）
// = 3_000 筆（U 為估算值，未實測）；門檻 100_000 約為其 33 倍，超過代表異常成長.
const dailyLimitWarnAbove = 100000

var errWebhookBodyTooLarge = errors.New("webhook body exceeds size limit")

// AlertHistoryEraser [F241-11] 刪除使用者的警報推播紀錄（由 alert 套件實作；這裡不 import alert）.
type AlertHistoryEraser interface {
	ForgetRecipient(ctx context.Context, userID string) error
}

// WebhookHandler LINE Webhook 處理器.
type WebhookHandler struct {
	line          *line.Client // 回覆、推播、下載圖片
	userRepo      UserStore
	weatherLookup *weather.Lookup
	alertSubRepo  SubscriptionStore
	convManager   *conversation.Manager
	geocoder      geocoding.Geocoder // 可為 nil（降級：不做 geocoding）
	weatherRepo   DistrictLister
	advisor       *weather.Advisor // 可為 nil（不附建議）
	nutrition     *nutrition.Analyzer
	mealRepo      meal.MealRepository
	limiter       *redis.Client
	daily         *localcache.Counter // [F24-32] 每日次數上限的本地計數（Redis 沒設定或出錯時用，不淘汰）
	clock         func() time.Time    // 僅供測試注入；nil 時用 time.Now
	workoutRepo   WorkoutStore
	profileRepo   ProfileStore
	aiParser      *fitness.AIParser
	liffURL       string // 空字串表示未啟用 LIFF
	assistant     *assistant.Assistant
	locations     *weather.UserLocations
	recent        *recentmeal.Store  // 剛記錄的那一餐（份量、補照片、店家等後續操作用）
	erased        *erasures          // 最近刪除資料的時間（擋住刪除前送出、刪除後才完成的寫入）
	alertHistory  AlertHistoryEraser // [F241-11] 可為 nil（沒有警報推播紀錄可清）
	work          *background.Group  // 背景處理中的事件；關機時等它們做完
}

// 背景處理的上限：同時處理的批次與「處理中 + 排隊中」的批次（每批最大 1 MB）.
const (
	maxConcurrentBatches = 50
	maxQueuedBatches     = 200
)

func newEventWork() *background.Group {
	return background.New(maxConcurrentBatches, maxQueuedBatches)
}

// NewWebhookHandler 建立 Webhook 處理器.
func NewWebhookHandler(
	lineClient *line.Client,
	userRepo UserStore,
	weatherLookup *weather.Lookup,
	alertSubRepo SubscriptionStore,
	convManager *conversation.Manager,
) *WebhookHandler {
	return &WebhookHandler{
		line:          lineClient,
		userRepo:      userRepo,
		weatherLookup: weatherLookup,
		alertSubRepo:  alertSubRepo,
		convManager:   convManager,
		erased:        newErasures(nil),
		daily:         localcache.NewCounter("daily_limit", dailyLimitWarnAbove),
		work:          newEventWork(),
	}
}

// NewWebhookHandlerWithOptions 建立 Webhook 處理器（含 geocoder 與 weatherRepo 選項）.
func NewWebhookHandlerWithOptions(
	lineClient *line.Client,
	userRepo UserStore,
	weatherLookup *weather.Lookup,
	alertSubRepo SubscriptionStore,
	convManager *conversation.Manager,
	geocoder geocoding.Geocoder,
	weatherRepo DistrictLister,
) *WebhookHandler {
	return &WebhookHandler{
		line:          lineClient,
		userRepo:      userRepo,
		weatherLookup: weatherLookup,
		alertSubRepo:  alertSubRepo,
		convManager:   convManager,
		geocoder:      geocoder,
		weatherRepo:   weatherRepo,
		erased:        newErasures(nil),
		daily:         localcache.NewCounter("daily_limit", dailyLimitWarnAbove),
		work:          newEventWork(),
	}
}

// WithAdvisor 設定天氣建議產生器.
func (h *WebhookHandler) WithAdvisor(advisor *weather.Advisor) *WebhookHandler {
	h.advisor = advisor
	return h
}

// HandleWebhook 接收 LINE Webhook 事件.
func (h *WebhookHandler) HandleWebhook(c echo.Context) error {
	body, err := readWebhookBody(c.Request().Body)
	if err != nil {
		if errors.Is(err, errWebhookBodyTooLarge) {
			return c.JSON(http.StatusRequestEntityTooLarge, httputil.ErrorWithCode(
				httputil.ErrorCodeInvalidParameter, "Request body too large"))
		}
		return c.JSON(http.StatusBadRequest, httputil.ErrorWithCode(
			httputil.ErrorCodeInvalidParameter, "Failed to read request body"))
	}

	// 非同步處理，立即回覆 200（LINE 要求幾秒內回覆）. 關機中或排隊已滿時回 503：
	// 在 LINE Developers 開啟「Webhook redelivery」的話 LINE 會稍後重送.
	if !h.work.Go(func(ctx context.Context) { h.processEvents(ctx, body) }) {
		slog.Warn("webhook not accepted: shutting down or too many queued")
		return c.JSON(http.StatusServiceUnavailable, httputil.ErrorWithCode(httputil.ErrorCodeServerError, "Busy, please retry"))
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// Shutdown 不再接受新的 webhook，等處理中與排隊中的事件做完；ctx 到期時取消剩下的（AI 呼叫等會中止）.
func (h *WebhookHandler) Shutdown(ctx context.Context) error {
	return h.work.Shutdown(ctx)
}

func readWebhookBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxWebhookBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxWebhookBodyBytes {
		return nil, errWebhookBodyTooLarge
	}
	return body, nil
}

func (h *WebhookHandler) processEvents(ctx context.Context, body []byte) {
	var wrapper models.EventWrapper
	if err := json.Unmarshal(body, &wrapper); err != nil {
		slog.Error("parse webhook events", "error", err)
		return
	}

	// 看圖分析最多 40 秒，整體要在 LINE reply token 失效前完成；關機逾時時 ctx 會被取消.
	ctx, cancel := context.WithTimeout(ctx, 55*time.Second)
	defer cancel()

	for i := range wrapper.Events {
		h.handleEventSafely(ctx, &wrapper.Events[i])
	}
}

// welcomeMessage 加好友時的歡迎訊息.
const welcomeMessage = "嗨，我是生活小幫手 👋\n" + helpMessage

// welcome 加好友：建立使用者並說明功能.
func (h *WebhookHandler) welcome(ctx context.Context, userID, replyToken string) {
	h.saveUser(ctx, userID)
	_ = h.line.ReplyText(ctx, replyToken, welcomeMessage, helpQuickReplies()...)
}

// stopNotifications 被封鎖：停止所有通知（推不到也會白白嘗試）. 其他資料依保留政策處理.
func (h *WebhookHandler) stopNotifications(ctx context.Context, userID string) {
	if h.alertSubRepo == nil {
		return
	}
	if err := h.alertSubRepo.DeleteByUser(ctx, userID); err != nil {
		slog.Error("stop notifications after unfollow", "error", err)
	}
}

// groupNotice 被加進群組或聊天室時的說明（回覆免費，不佔推播額度）.
const groupNotice = "我目前只支援一對一聊天，請加我好友後私訊我 😊"

// handleEventSafely 處理一則事件；任何 panic 只影響這一則，不會讓整個服務結束.
func (h *WebhookHandler) handleEventSafely(ctx context.Context, event *models.Event) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic while handling LINE event", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	if event.Type == "join" {
		_ = h.line.ReplyText(ctx, event.ReplyToken, groupNotice)
		return
	}
	// 只處理一對一聊天：群組裡每則訊息都處理的話，沒對上的會全部送去付費的 AI；
	// 沒有 userId 的訊息也不能處理，否則不同人的資料會混在空字串底下.
	if event.Source.Type != "user" || event.Source.UserID == "" {
		return
	}
	switch event.Type {
	case "follow":
		// [F291-05] saveUser 的寫後再查要比對事件時間，加好友路徑也要帶.
		h.welcome(withSentAt(ctx, event.Timestamp), event.Source.UserID, event.ReplyToken)
		return
	case "unfollow":
		h.stopNotifications(ctx, event.Source.UserID)
		return
	case "message":
	default:
		return
	}

	// [F291-05] 先帶上 LINE 事件時間：saveUser 的寫後再查要知道這則訊息是什麼時候送出的.
	ctx = withSentAt(ctx, event.Timestamp)
	// 儲存使用者（同步執行，processEvents 本身已在 goroutine 中）
	saveCtx, saveCancel := context.WithTimeout(ctx, 5*time.Second)
	h.saveUser(saveCtx, event.Source.UserID)
	saveCancel()

	h.handleMessage(ctx, event)
}

// osmAttribution OpenStreetMap 資料授權要求的來源標示.
const osmAttribution = "（資料來源：OpenStreetMap contributors）"

func (h *WebhookHandler) saveUser(ctx context.Context, userID string) {
	if userID == "" {
		return
	}
	user := &userstore.User{
		ID:         userID,
		CreateTime: time.Now(),
	}
	err := h.userRepo.Upsert(ctx, user)
	// [F291-05] 寫後再查（回錯也查）：刪除後落地的 user 文件補刪；沒有 replyToken，補刪失敗只記 Error（helper 內已記）.
	erased, undoErr := h.recheckAfterWrite(ctx, userID, func(cctx context.Context) error {
		return h.userRepo.Delete(cctx, userID)
	})
	if erased {
		slog.Info("dropped user save after data erasure", "undo_failed", undoErr != nil)
		return
	}
	if err != nil {
		slog.Error("save user", "error", redact.Error(err))
	}
}
