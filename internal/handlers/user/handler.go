package user

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.uber.org/zap"

	userdomain "chatbot-go/internal/domain/user"
)

type Handler interface {

	// GetUser 獲取使用者詳細資訊
	//
	// 參數
	// - c echo.Context: Echo context，包含HTTP請求與回應的相關資訊
	//
	// 返回
	// - error: 處理過程中可能發生的錯誤
	//
	// HTTP回應
	// - 200 OK: 使用者詳細資訊
	// - 400 Bad Request: 請求內容格式錯誤或缺少必要資料
	// - 401 Unauthorized: 使用者未登入或無權限
	// - 404 Not Found: 使用者不存在
	// - 500 Internal Server Error: 服務器內部處理錯誤
	GetUser(c echo.Context) error
}

type handler struct {
	// user 提供使用者相關的業務邏輯
	user userdomain.Service

	// logger 用於記錄操作和錯誤
	logger *zap.Logger
}

func NewHandler(
	user userdomain.Service,
	logger *zap.Logger,
) Handler {
	return &handler{
		user:   user,
		logger: logger.Named("Handler").Named("User"),
	}
}

func (h *handler) GetUser(c echo.Context) error {

	h.logger.Info("get user handler")

	id := c.Param("id")

	if id == "" {
		h.logger.Error("failed to get user id")
		return echo.NewHTTPError(http.StatusBadRequest, "failed to get user id")
	}

	userID, err := bson.ObjectIDFromHex(id)
	if err != nil {
		h.logger.Error("failed to parse user id", zap.Error(err))
		return echo.NewHTTPError(http.StatusBadRequest, "failed to parse user id")
	}

	user, err := h.user.GetUser(c.Request().Context(), userID)
	if err != nil {
		h.logger.Error("failed to get user", zap.Error(err))
		return err
	}

	return c.JSON(http.StatusOK, user)
}
