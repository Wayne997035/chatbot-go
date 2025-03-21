package webhookdoamin

import (
	"bytes"
	"chatbot-go/internal/models"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"

	"go.uber.org/zap"
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
	WebhookService(c echo.Context, body []byte) error
}

type service struct {
	logger *zap.Logger
}

func NewService(logger *zap.Logger) Service {
	return &service{
		logger: logger,
	}
}

// LINE Messaging API 回覆 URL 與 Channel Token（請替換為實際資料）
const lineReplyURL = "https://api.line.me/v2/bot/message/reply"
const channelToken = "UShFzDZGIRVE0Wn8+b7ZntGhgV3yOjWI+VmMnIEAzhmdMfJOfYQb/Ufcqcwqr34Z7peiEUByXd8Z+0SZbMu1mhiint6UpTL8+FXU+zpVl/3Er/f33Id38UnbFc9aIbMA/U3T1jHifNZOHPbhwvej5QdB04t89/1O/w1cDnyilFU="

func (s *service) WebhookService(c echo.Context, body []byte) error {
	fmt.Println("收到line webhook的訊息")

	// 先回覆 line ok, 再處理 line webhook
	c.JSON(http.StatusOK, map[string]string{"message": "OK"})

	// 回复line Webhook request
	go func(body []byte) {

		lineWebhook := &models.LineWebHook{}

		s.logger.Info("body = ", zap.String("body", string(body)))

		if err := json.Unmarshal(body, lineWebhook); err != nil {
			s.logger.Error("json.Unmarshal error:", zap.Error(err))
		}

		for _, event := range lineWebhook.Events {
			if event.Type == "message" {
				s.replyMessage(event.ReplyToken, event.Message.Text)
			}
		}

		return
	}(body)

	return nil
}

func (s *service) replyMessage(replyToken, message string) {
	fmt.Println("Reply message received!")
	// 回覆line Webhook request
	replyBody := &models.LineResponse{
		ReplyToken: replyToken,
		Message: []models.Message{
			{
				Type: "text",
				Text: message,
			},
		},
	}

	jsonData, err := json.Marshal(replyBody)
	s.logger.Info("jsonData = ", zap.String("jsonData", string(jsonData)))
	if err != nil {
		fmt.Println("JSON Marshal error:", err)
		s.logger.Error("json.Marshal error:", zap.Error(err))
		return
	}

	req, err := http.NewRequest("POST", lineReplyURL, bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Println("NewRequest error:", err)
		s.logger.Error("NewRequest error:", zap.Error(err))
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+channelToken)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("發送回覆失敗:", err)
		s.logger.Error("send reply error:", zap.Error(err))
		return
	}
	defer resp.Body.Close()
	fmt.Println("回覆訊息成功:", resp.Status)
	s.logger.Info("send reply success", zap.String("status", resp.Status))
}
