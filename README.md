# Chatbot-Go

一個基於 Golang 和 MongoDB 的輕量級高效能聊天機器人服務。

## 專案概述

Chatbot-Go 是一個使用 Go 語言開發的聊天機器人後端服務，利用 MongoDB 作為持久化存儲層。本專案旨在提供一個輕量級、高效能的聊天解決方案，可以輕鬆擴展和集成到各種應用程序中。

### 主要特點

- **輕量級架構**: 專注於核心功能，避免不必要的複雜性
- **高效能**: 利用 Go 的並發能力和 MongoDB 的高性能存儲
- **可擴展性**: 模塊化設計，便於後續功能擴展
- **簡單易用**: 提供清晰的 API 和良好的文檔

## 技術

- **Go 1.24+**: 利用最新的 Go 語言特性
- **MongoDB v2 Driver**: 使用官方 MongoDB 驅動程序的第二版本
- **Echo**: 高性能、可擴展的 Web 框架
- **Wire**: Google 的依賴注入工具，實現清晰的依賴關係
- **自定義日誌系統**: 為不同的運行環境提供適當的日誌級別

## 快速開始

### 前置條件

- Go 1.24 或更高版本
- MongoDB 5.0 或更高版本
- 基本的 Go 開發環境

### 安裝

1. 克隆存儲庫

```bash
git clone https://github.com/koopa0/chatbot-go.git
cd chatbot-go
```


根據您的環境修改這些值。

4. 編譯專案

```bash
go build -o chatbot-go
```

5. 運行應用

```bash
./chatbot-go
```

服務將在指定的端口（默認為 8080）上啟動。


## 核心功能

目前實現的功能：

- 基本的聊天機器人對話功能
- 消息的持久化存儲
- 簡單的用戶身份驗證
- REST API 接口

計劃中的功能：

- 會話上下文管理
- 自定義回答模板
- 機器學習集成
- WebSocket 支持
- 多語言支持
- 訊息統計和分析

## API 文檔

### 認證

所有 API 調用都需要在標頭中使用 Bearer 令牌：

```
Authorization: Bearer <your-api-token>
```

### 端點

#### 發送消息

```
POST /api/v1/messages
```

請求正文：

```json
{
  "user_id": "123",
  "content": "你好，機器人！",
  "session_id": "abc-123"
}
```

回應：

```json
{
  "id": "60f1a5c7e6b3f12d3c4a5b6c",
  "content": "你好！有什麼我可以幫你的嗎？",
  "timestamp": "2023-07-16T12:34:56Z"
}
```

更多 API 文檔將隨著專案發展而更新。

## 開發

### 生成 Wire 依賴

```bash
cd wire
wire
```

### 運行測試

```bash
go test ./...
```

### 代碼風格

此專案遵循標準的 Go 代碼風格和最佳實踐。使用 `gofmt` 和 `golint` 來確保代碼質量。

```bash
go fmt ./...
golint ./...
```

## 部署

### Docker

提供了 Dockerfile 來幫助容器化應用程序：

```bash
docker build -t chatbot-go .
docker run -p 8080:8080 --env-file .env chatbot-go
```

### Kubernetes

將在後續版本中提供 Kubernetes 配置示例。

## 性能考量

- 使用連接池來管理 MongoDB 連接
- 實現適當的緩存機制減少數據庫訪問
- 使用 Go 的 goroutines 進行並發處理
- 監控系統資源使用情況

## 貢獻

我們歡迎您的貢獻！請先討論您想做的更改，然後提交拉取請求。

## 許可證

此專案採用 MIT 許可證 - 查看 LICENSE 文件了解詳情。

© 2025 Chatbot-Go Team# chatbot-go
