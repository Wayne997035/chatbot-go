package userdomain

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.uber.org/zap"

	"chatbot-go/internal/models"
)

var _ Service = (*service)(nil)

type Service interface {
	// GetUser 獲取使用者詳情
	//
	// 參數:
	// - ctx: 操作上下文，包含請求跟踪資訊
	// - id: 使用者ID
	//
	// 返回:
	// - *models.User: 使用者詳細資訊
	// - error: 可能的錯誤，如用戶不存在(ErrUserNotFound)
	GetUser(ctx context.Context, id bson.ObjectID) (*models.User, error)
}

type service struct {
	userRepo Repository
	logger   *zap.Logger
}

func NewService(logger *zap.Logger, repo Repository) Service {
	return &service{
		userRepo: repo,
		logger:   logger,
	}
}

func (s *service) GetUser(ctx context.Context, id bson.ObjectID) (*models.User, error) {

	s.logger.Info("get user service")

	return s.userRepo.FindByID(ctx, id)
}
