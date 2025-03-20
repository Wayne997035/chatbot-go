package userdomain

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/zap"

	"chatbot-go/internal/models"
)

var _ Repository = &repository{}

type Repository interface {
	FindByID(ctx context.Context, id bson.ObjectID) (*models.User, error)
}

func NewRepository(logger *zap.Logger, collection *mongo.Collection) Repository {
	return &repository{
		collection: collection,
		logger:     logger,
	}
}

type repository struct {
	collection *mongo.Collection
	logger     *zap.Logger
}

func (r *repository) FindByID(ctx context.Context, id bson.ObjectID) (*models.User, error) {

	r.logger.Info("find user by id repository")

	user := &models.User{}

	if err := r.collection.FindOne(ctx, bson.M{"_id": id}).Decode(user); err != nil {
		r.logger.Error("failed to find user by id", zap.Error(err))
		return nil, err
	}

	r.logger.Info("user found repository")

	return user, nil
}
