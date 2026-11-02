// Command richmenu 建立 LINE 聊天室的圖片選單並設為所有使用者的預設選單.
//
// 用法：LINE_CHANNEL_TOKEN=... go run ./cmd/richmenu
// 金鑰只從環境變數讀取，不會印出.
package main

import (
	"chatbot-go/internal/richmenu"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	token := os.Getenv("LINE_CHANNEL_TOKEN")
	if token == "" {
		log.Fatal("請設定環境變數 LINE_CHANNEL_TOKEN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	client := &richmenu.Client{
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Token:    token,
		APIBase:  richmenu.DefaultAPIBase,
		DataBase: richmenu.DefaultDataBase,
	}
	id, err := client.Setup(ctx)
	cancel()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("rich menu is live:", id)
}
