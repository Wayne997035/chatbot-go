package user

import (
	"chatbot-go/internal/httputil"
	"net/http"

	"github.com/labstack/echo/v4"

	userstore "chatbot-go/internal/storage/database/user"
)

// UserHandler 使用者處理器.
type UserHandler struct {
	userRepo userstore.UserRepository
}

// NewUserHandler 建立使用者處理器.
func NewUserHandler(userRepo userstore.UserRepository) *UserHandler {
	return &UserHandler{userRepo: userRepo}
}

// GetAllUsers 取得所有使用者.
func (h *UserHandler) GetAllUsers(c echo.Context) error {
	users, err := h.userRepo.FindAll(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, httputil.ErrorWithCode(
			httputil.ErrorCodeServerError, "Failed to get users"))
	}
	return httputil.OK(c, users)
}
