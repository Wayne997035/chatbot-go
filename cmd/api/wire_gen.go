// Code generated by Wire. DO NOT EDIT.

//go:generate go run -mod=mod github.com/google/wire/cmd/wire
//go:build !wireinject
// +build !wireinject

package main

import (
	"chatbot-go/internal/config"
	"chatbot-go/internal/domain/user"
	"chatbot-go/internal/domain/webhook"
	"chatbot-go/internal/driver"
	"chatbot-go/internal/handlers/user"
	"chatbot-go/internal/handlers/webhook"
	"chatbot-go/internal/server"
	"github.com/google/wire"
)

// Injectors from wire.go:

func InitializeChatbot() (*server.Server, error) {
	logger := config.NewLogger()
	configConfig := config.NewConfig(logger)
	client := driver.ConnectMongo(configConfig)
	collection := driver.NewUsersCollection(client, configConfig)
	repository := userdomain.NewRepository(logger, collection)
	service := userdomain.NewService(logger, repository)
	handler := user.NewHandler(service, logger)
	lineConfig := config.NewLineConfig(logger)
	webhookdoaminService := webhookdoamin.NewService(logger, lineConfig)
	webhookHandler := webhook.NewHandler(webhookdoaminService, lineConfig, logger)
	serverServer := server.NewServer(handler, webhookHandler, logger)
	return serverServer, nil
}

// wire.go:

var ConfigSet = wire.NewSet(config.NewLogger, config.NewConfig, config.NewLineConfig)

var MongoSet = wire.NewSet(driver.ConnectMongo, driver.NewUsersCollection)

var UserSet = wire.NewSet(userdomain.NewRepository, userdomain.NewService, webhookdoamin.NewService, user.NewHandler, webhook.NewHandler)

var ServerSet = wire.NewSet(server.NewServer)

var ApplicationSet = wire.NewSet(
	ConfigSet,
	MongoSet,
	UserSet,
	ServerSet,
)
