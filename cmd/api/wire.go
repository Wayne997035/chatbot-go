//go:build wireinject
// +build wireinject

package main

import (
	"github.com/google/wire"

	"chatbot-go/internal/config"
	userdomain "chatbot-go/internal/domain/user"
	webhookdomain "chatbot-go/internal/domain/webhook"
	"chatbot-go/internal/driver"
	"chatbot-go/internal/handlers/user"
	"chatbot-go/internal/handlers/webhook"
	"chatbot-go/internal/server"
)

var ConfigSet = wire.NewSet(
	config.NewLogger,
	config.NewConfig,
)

var MongoSet = wire.NewSet(
	driver.ConnectMongo,
	driver.NewUsersCollection,
)

var UserSet = wire.NewSet(
	userdomain.NewRepository,
	userdomain.NewService,
	webhookdomain.NewService,
	user.NewHandler,
	webhook.NewHandler,
)

var ServerSet = wire.NewSet(
	server.NewServer,
)

var ApplicationSet = wire.NewSet(
	ConfigSet,
	MongoSet,
	UserSet,
	ServerSet,
)

func InitializeChatbot() (*server.Server, error) {
	wire.Build(ApplicationSet)
	return &server.Server{}, nil
}
