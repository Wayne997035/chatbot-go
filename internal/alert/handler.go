package alert

import (
	"chatbot-go/internal/httputil"
	"chatbot-go/internal/storage/database/alertsub"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

// AlertHandler 警報訂閱 API 處理器.
type AlertHandler struct {
	alertSubRepo alertsub.AlertSubRepository
}

// NewAlertHandler 建立警報訂閱處理器.
func NewAlertHandler(repo alertsub.AlertSubRepository) *AlertHandler {
	return &AlertHandler{alertSubRepo: repo}
}

type subscribeRequest struct {
	UserID    string   `json:"userId"`
	AlertType string   `json:"alertType"`
	Regions   []string `json:"regions"`
}

// GetSubscriptions GET /api/v1/alerts/subscriptions/:userID.
func (h *AlertHandler) GetSubscriptions(c echo.Context) error {
	userID := strings.TrimSpace(c.Param("userID"))
	if userID == "" {
		return c.JSON(http.StatusBadRequest, httputil.ErrorWithCode(
			httputil.ErrorCodeInvalidParameter, "userID is required"))
	}

	subs, err := h.alertSubRepo.FindByUserID(c.Request().Context(), userID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, httputil.ErrorWithCode(
			httputil.ErrorCodeServerError, "Failed to get subscriptions"))
	}

	return httputil.OK(c, subs)
}

// Subscribe POST /api/v1/alerts/subscriptions.
func (h *AlertHandler) Subscribe(c echo.Context) error {
	var req subscribeRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, httputil.ErrorWithCode(
			httputil.ErrorCodeInvalidParameter, "Invalid request body"))
	}

	req.UserID = strings.TrimSpace(req.UserID)
	req.AlertType = strings.TrimSpace(req.AlertType)

	if req.UserID == "" {
		return c.JSON(http.StatusBadRequest, httputil.ErrorWithCode(
			httputil.ErrorCodeInvalidParameter, "userId is required"))
	}

	if req.AlertType != alertsub.TypeEarthquakeTsunami && req.AlertType != alertsub.TypeWeatherWarning {
		return c.JSON(http.StatusBadRequest, httputil.ErrorWithCode(
			httputil.ErrorCodeInvalidParameter,
			fmt.Sprintf("alertType must be %q or %q", alertsub.TypeEarthquakeTsunami, alertsub.TypeWeatherWarning)))
	}

	if req.AlertType == alertsub.TypeWeatherWarning && len(req.Regions) == 0 {
		return c.JSON(http.StatusBadRequest, httputil.ErrorWithCode(
			httputil.ErrorCodeInvalidParameter, "regions is required for weather_warning"))
	}

	now := time.Now()
	sub := &alertsub.AlertSubscription{
		ID:         fmt.Sprintf("%s_%s", req.UserID, req.AlertType),
		UserID:     req.UserID,
		AlertType:  req.AlertType,
		Regions:    req.Regions,
		Enabled:    true,
		CreateTime: now,
		UpdateTime: now,
	}

	if err := h.alertSubRepo.Upsert(c.Request().Context(), sub); err != nil {
		return c.JSON(http.StatusInternalServerError, httputil.ErrorWithCode(
			httputil.ErrorCodeServerError, "Failed to save subscription"))
	}

	return httputil.OK(c, sub)
}

// Unsubscribe DELETE /api/v1/alerts/subscriptions/:userID/:type.
func (h *AlertHandler) Unsubscribe(c echo.Context) error {
	userID := strings.TrimSpace(c.Param("userID"))
	alertType := strings.TrimSpace(c.Param("type"))

	if userID == "" || alertType == "" {
		return c.JSON(http.StatusBadRequest, httputil.ErrorWithCode(
			httputil.ErrorCodeInvalidParameter, "userID and type are required"))
	}

	id := fmt.Sprintf("%s_%s", userID, alertType)
	if err := h.alertSubRepo.Delete(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusInternalServerError, httputil.ErrorWithCode(
			httputil.ErrorCodeServerError, "Failed to delete subscription"))
	}

	return httputil.Success(c, "unsubscribed")
}
