# chatbot-go

LINE Bot 天氣查詢機器人，以 Go 開發，串接中央氣象署 CWA Open Data API，提供台灣各縣市鄉鎮區的即時天氣預報查詢，並支援災害警報推播通知。

## 功能

### 天氣查詢
- **文字查詢** -- 使用者輸入區域名稱（如「信義區」），回傳該區域天氣預報
- **位置查詢** -- 使用者傳送 LINE 位置訊息，自動解析地址並回傳當地天氣
- **排程同步** -- 透過 cron 定時從 CWA Open Data API 抓取全台 22 縣市天氣資料
- **手動觸發** -- 提供 API endpoint 手動觸發天氣資料同步
- **Redis 快取** -- cache-aside pattern，減少 MongoDB 查詢負擔，TTL 1 小時

### 災害警報推播
- **天氣特報** -- 當 CWA 發布大雨/豪雨特報時，推播通知給訂閱該地區的使用者
- **地震速報** -- 地震報告發布時，推播通知給有開啟地震警告的使用者
- **海嘯警報** -- 海嘯警報發布時（非綠色解除），推播通知給有開啟地震海嘯警告的使用者
- **排程檢查** -- 每 5 分鐘檢查 CWA API，Redis dedup 避免重複推播
- **訂閱管理** -- 使用者透過 LINE 關鍵字或 REST API 訂閱/取消

### 其他
- **機敏資料保護** -- 採用輪替金鑰機制確保機敏資料安全。

## 安全架構

本專案採用金鑰輪替架構來處理機敏資料的加密與解密，確保配置與金鑰分離。
目前改用 Tink 管理機敏設定，支援金鑰輪替。

## 技術架構

| 項目 | 技術 |
|------|------|
| 語言 | Go 1.24.1 |
| HTTP 框架 | Echo v4 |
| 資料庫 | MongoDB (mongo-driver v2) |
| 快取 | Redis (go-redis v9) |
| 排程 | gocron v2 |
| 日誌 | log/slog + lumberjack |
| 設定 | YAML + 環境變數替換 |
| 容器 | Docker multi-stage build (Alpine) |
| Lint | golangci-lint v2 (23 linters) |
| CI/CD | GitHub Actions |

## 專案結構

```
cmd/api/main.go                          # 進入點
configs/                                 # 設定檔與環境變數
internal/
  platform/                              # 基礎設施層 (config, logger, driver, server...)
  storage/database/                      # 資料存取層 (repositories)
  webhook/                               # LINE webhook 處理
  alert/                                 # 災害警報模組
  conversation/                          # 對話狀態管理
  weather/                               # 天氣業務邏輯
  user/                                  # 使用者管理
  crypto/                                # Tink 機敏設定保護
  httputil/                              # HTTP 錯誤回應工具
  models/                                # 外部 API 結構定義
tests/integration/                       # 整合測試
build/                                   # Taskfile, Dockerfile, docker-compose
.github/workflows/                       # GitHub Actions CI/CD
```

## API Endpoints

| Method | Path | 說明 |
|--------|------|------|
| GET | `/health` | Health check |
| POST | `/api/v1/webhooks` | LINE webhook（HMAC-SHA256 驗證） |
| GET | `/api/v1/users` | 查詢所有使用者 |
| GET | `/api/v1/openDataUpdate` | 手動觸發天氣資料同步 |
| GET | `/api/v1/alerts/subscriptions/:userID` | 查詢使用者警報訂閱 |
| POST | `/api/v1/alerts/subscriptions` | 建立/更新警報訂閱 |
| DELETE | `/api/v1/alerts/subscriptions/:userID/:type` | 取消警報訂閱 |

## LINE Bot 操作說明

### 天氣查詢

直接輸入區域名稱即可查詢天氣：

| 輸入 | 回覆 |
|------|------|
| `信義區` | 信義區天氣預報（可能有多個同名區域） |
| `台北市信義區` | 精確查詢台北市信義區 |
| 傳送位置訊息 | 自動解析地址回傳當地天氣 |

### 災害警報訂閱

透過 LINE 聊天室輸入以下關鍵字管理訂閱：

| 關鍵字 | 動作 | 說明 |
|--------|------|------|
| `開啟地震海嘯警告` | 訂閱 | 全台地震速報 + 海嘯警報推播 |
| `關閉地震海嘯警告` | 取消 | 停止推播 |
| `開啟天氣特報` | 訂閱 | 進入地區選擇流程（多輪對話） |
| `關閉天氣特報` | 取消 | 停止天氣特報推播 |

### 天氣特報地區選擇流程

```
使用者：開啟天氣特報
Bot：  請輸入您想訂閱的縣市名稱（可多個），輸入「完成」結束選擇：
       台北市、新北市、桃園市、台中市、台南市、高雄市...

使用者：台北市
Bot：  已加入「台北市」，還有嗎？輸入「完成」結束選擇

使用者：新北市
Bot：  已加入「新北市」，還有嗎？輸入「完成」結束選擇

使用者：完成
Bot：  已開啟天氣特報通知！
       訂閱地區：台北市、新北市
       當這些地區有天氣特報時，將立即通知您。
```

結束對話的關鍵字：`完成`、`好了`、`不用了`、`結束`


| 按鈕 | Action Type | Text |
|------|-------------|------|
| 地震海嘯警告 開啟 | message | `開啟地震海嘯警告` |
| 地震海嘯警告 關閉 | message | `關閉地震海嘯警告` |
| 天氣特報 開啟 | message | `開啟天氣特報` |
| 天氣特報 關閉 | message | `關閉天氣特報` |

4. 設為預設 Rich Menu（或透過 API 指定）

### REST API 訂閱管理（LIFF 用）

```bash
# 查詢訂閱
GET /api/v1/alerts/subscriptions/{userID}

# 建立訂閱
POST /api/v1/alerts/subscriptions
Content-Type: application/json
{
  "userId": "U1234567890abcdef",
  "alertType": "weather_warning",
  "regions": ["台北市", "新北市"]
}

# alertType: "earthquake_tsunami" | "weather_warning"
# earthquake_tsunami 不需要 regions（全台）

# 取消訂閱
DELETE /api/v1/alerts/subscriptions/{userID}/{alertType}
```

## CWA Open Data API

### 天氣預報

資料來源: opendata.cwa.gov.tw

擷取的氣象要素：

| 代碼 | 說明 | 單位 |
|------|------|------|
| T | 平均溫度 | 度C |
| Td | 平均露點溫度 | 度C |
| AT | 體感溫度 | 度C |
| RH | 平均相對濕度 | % |
| PoP6h / PoP12h | 降雨機率 | % |
| WS | 風速 | - |
| WD | 風向 | - |
| Wx | 天氣現象 | - |
| CI | 紫外線指數 | - |
| WeatherDescription | 天氣預報綜合描述 | - |

### 災害警報

| 資料集 | 說明 | 排程 |
|--------|------|------|------|
| 天氣警特報 | 各縣市目前天氣警特報狀態 | 每 5 分鐘 |
| 地震報告 | 有感地震報告（limit=1 取最新） | 每 5 分鐘 |
| 海嘯警報 | 海嘯資訊（過濾綠色=解除） | 每 5 分鐘 |

CWA API 授權金鑰需至 [CWA Open Data 平台](https://opendata.cwa.gov.tw) 註冊取得。

## 開發

### 前置需求

- Go 1.24+
- MongoDB
- Redis（選用，無 Redis 仍可運行，但多輪對話和 dedup 功能需要 Redis）
- [Task](https://taskfile.dev/) (go-task)
- golangci-lint v2

### 本機執行

```bash
# 啟動 MongoDB + Redis
cd build && docker compose up mongo redis -d

# 執行
cd build && task run
```

### 程式碼品質檢查

```bash
cd build

# lint + 單元測試
task check

# lint + 單元測試 + 整合測試
task check:all

# 只跑整合測試
task check:integration
```

`task check` 包含：go vet、go mod tidy、golangci-lint（23 linters）、單元測試。

全部通過才能上線。

### Build Docker Image

```bash
cd build

# 自動跑 check:all 後 build（測試不過不會 build）
task build
```

## 環境設定

透過 `APP_ENV` 環境變數選擇設定檔：

| APP_ENV | 設定檔 | 說明 |
|---------|--------|------|
| local（預設） | configs/local.yaml | 本機開發，直接寫值 |
| production | configs/production.yaml | 正式環境，`${ENV_VAR}` 替換 |

正式環境需要的環境變數：

| 變數 | 說明 |
|------|------|
| `MONGO_URI` | MongoDB 連線字串 |
| `REDIS_ADDR` | Redis 地址 |
| `REDIS_PASSWORD` | Redis 密碼 |
| `LINE_CHANNEL_SECRET` | LINE Channel Secret |
| `LINE_CHANNEL_TOKEN` | LINE Channel Token |
| `CWA_AUTH_KEY` | CWA Open Data API 授權金鑰 |
| `CRYPTO_KEYSET` | 金鑰集合字串（供輪替） |

## CI/CD

GitHub Actions 流程（`.github/workflows/ci.yml`）：

```
push (main / develop / feature/** / fix/**)
         |
         v
check (lint + unit tests)
         |
         v
integration (integration tests, MongoDB service container)
         |
         v
build (Docker image, only on main/develop branch)
```

測試全部通過才會進行 build。build 失敗不會產出 image。

## DI 模式

採用 manual constructor injection，不使用 DI 框架（Wire 已 archived）：

```
main.go
  -> config.Load()
  -> driver.ConnectMongo() / ConnectRedis()
  -> database.NewRepositories()
  -> weather.NewLookup(repo, redisClient)
  -> webhook.NewWebhookHandler(userRepo, weatherLookup, alertSubRepo, convManager)
  -> alert.NewChecker(alertSubRepo, notifier, redisClient, cfg)
  -> alert.NewScheduler(checker, cfg)
  -> server.Start()
```

## 測試

### 單元測試

| 測試檔案 | 涵蓋範圍 |
|----------|----------|
| crypto/aes_test.go | Tink 機敏設定保護流程、無效 keyset、錯誤 keyset |
| middleware/signature_test.go | LINE webhook HMAC-SHA256 簽章驗證 |
| weather/api_client_test.go | CWA 資料解析、時間解析、最近時間選取 |
| weather/formatter_test.go | 天氣預報格式化、多筆格式化、空值處理 |
| webhook/address_parser_test.go | 中文地址解析（市 / 縣 / 區 / 鎮 / 鄉） |
| webhook/keyword_handler_test.go | 關鍵字路由、區域驗證、結束關鍵字 |
| alert/cwa_client_test.go | CWA 警特報/地震 API 回應解析 |
| alert/checker_test.go | Redis nil 降級、dedup 邏輯 |

### 整合測試

| 測試檔案 | 涵蓋範圍 |
|----------|----------|
| tests/integration/weather_integration_test.go | MongoDB CRUD、CWA API mock + FetchAndStore 完整流程驗證 |
| tests/integration/alert_integration_test.go | 災害警報推播完整流程驗證 |

## 部署

### Docker Compose

```bash
cd build && docker compose up -d
```
