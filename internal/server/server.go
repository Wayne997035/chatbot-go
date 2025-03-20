package server

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"

	"chatbot-go/internal/handlers/user"
)

type Server struct {
	server *echo.Echo

	/**
	 Handler classes are defined in their respective packages.
	**/

	user user.Handler

	logger *zap.Logger
}

func NewServer(
	user user.Handler,
	logger *zap.Logger,
) *Server {
	return &Server{
		server: echo.New(),
		user:   user,
		logger: logger.Named("Server").Named("ChatBot"),
	}
}

func (s *Server) Start(address string) error {
	s.RegisterHandler()
	return s.server.Start(address)
}

func (s *Server) Run(port string) error {

	go func() {
		if err := s.Start(port); err != nil {
			s.logger.Error("failed to start server", zap.Error(err))
		}
	}()

	s.logger.Info("server started")
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	s.logger.Info("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := s.server.Shutdown(ctx); err != nil {
		s.logger.Error("Server forced to shutdown", zap.Error(err))
		return err
	}

	s.logger.Info("Server stopped gracefully")
	return nil
}

func (s *Server) RegisterHandler() {

	group := s.server.Group("/api/v1")

	group.GET("/users/:id", s.user.GetUser)
}
