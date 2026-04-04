# chatbot-go

LINE Bot 天氣查詢機器人，以 Go 開發，串接中央氣象署 CWA Open Data API，提供台灣各縣市鄉鎮區的即時天氣預報查詢。

## 功能

- **文字查詢** -- 使用者輸入區域名稱（如「信義區」），回傳該區域天氣預報
- **位置查詢** -- 使用者傳送 LINE 位置訊息，自動解析地址並回傳當地天氣
- **排程同步** -- 透過 cron 定時從 CWA Open Data API 抓取全台 22 縣市天氣資料
- **手動觸發** -- 提供 API endpoint 手動觸發天氣資料同步
- **Redis 快取** -- cache-aside pattern，減少 MongoDB 查詢負擔，TTL 1 小時
- **AES-GCM 加密** -- 提供敏感資料加解密工具

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
configs/
  local.yaml                             # 本機開發設定
  production.yaml                        # 正式環境設定（環境變數替換）
internal/
  platform/                              # 基礎設施層
    config/                              # 設定載入與驗證
    logger/                              # slog + lumberjack 日誌
    driver/                              # MongoDB / Redis 連線
    server/                              # Echo server + 路由 + graceful shutdown
    middleware/                          # LINE webhook HMAC-SHA256 簽章驗證
    health/                              # Health check endpoint
  storage/database/                      # 資料存取層
    user/                                # User repository (interface + impl)
    weather/                             # Weather repository (interface + impl)
    repositories.go                      # DI container
  webhook/                               # LINE webhook 處理
    handler.go                           # 事件分派（文字 / 位置 / 不支援類型）
    line_client.go                       # LINE Reply API 呼叫
    address_parser.go                    # 中文地址解析（市 / 縣 / 區 / 鎮 / 鄉）
  weather/                               # 天氣業務邏輯
    api_client.go                        # CWA Open Data API 呼叫與資料轉換
    lookup.go                            # 查詢（Redis cache-aside + MongoDB）
    formatter.go                         # 天氣預報格式化為 LINE 回覆文字
    scheduler.go                         # gocron 排程管理
    handler.go                           # 手動觸發同步 endpoint
  user/                                  # 使用者管理
    handler.go                           # 查詢所有使用者 endpoint
  crypto/                                # AES-GCM 加解密
  httputil/                              # HTTP 錯誤回應工具
  models/                                # 外部 API 結構定義
    line_event.go                        # LINE webhook 事件結構
    cwa.go                               # CWA API 回應結構
tests/integration/                       # 整合測試（需要 MongoDB）
build/
  Taskfile.yml                           # task check / build / run / clean
  Dockerfile                             # Multi-stage build
  docker-compose.yml                     # app + MongoDB + Redis
.golangci.yml                            # golangci-lint v2 設定
.github/workflows/ci.yml                 # GitHub Actions CI/CD
```

## API Endpoints

| Method | Path | 說明 |
|--------|------|------|
| GET | `/health` | Health check |
| POST | `/webhook` | LINE webhook（HMAC-SHA256 驗證） |
| GET | `/api/v1/users` | 查詢所有使用者 |
| POST | `/api/v1/weather/sync` | 手動觸發天氣資料同步 |

## CWA Open Data API

串接中央氣象署開放資料平台，資料集代碼 F-D0047-001 至 F-D0047-089（奇數），涵蓋全台 22 縣市鄉鎮天氣預報。

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

## 開發

### 前置需求

- Go 1.24+
- MongoDB
- Redis（選用，無 Redis 仍可運行）
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

# lint + 單元測試 + 整合測試（需要 MongoDB）
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

## CI/CD

GitHub Actions 流程（`.github/workflows/ci.yml`）：

```
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
  -> webhook.NewWebhookHandler(userRepo, weatherLookup)
  -> server.Start()
```

## 測試

### 單元測試

| 測試檔案 | 涵蓋範圍 |
|----------|----------|
| crypto/aes_test.go | AES-GCM 加解密 round-trip、無效金鑰、錯誤金鑰 |
| middleware/signature_test.go | LINE webhook HMAC-SHA256 簽章驗證 |
| weather/api_client_test.go | CWA 資料解析、時間解析、最近時間選取 |
| weather/formatter_test.go | 天氣預報格式化、多筆格式化、空值處理 |
| webhook/address_parser_test.go | 中文地址解析（市 / 縣 / 區 / 鎮 / 鄉） |

### 整合測試

| 測試檔案 | 涵蓋範圍 |
|----------|----------|
| tests/integration/weather_integration_test.go | MongoDB CRUD、CWA API mock + FetchAndStore 完整流程驗證 |

整合測試使用 `httptest.Server` mock CWA API，驗證從 API 呼叫到 MongoDB 存取的完整資料流。

## 部署

### Docker Compose

```bash
cd build && docker compose up -d
```

包含 app、MongoDB 8、Redis 7，皆有 healthcheck 設定。

### 手動 Docker Build

```bash
docker build -f build/Dockerfile -t chatbot-go:latest .
```

Dockerfile 使用 multi-stage build：golang:1.24.1-alpine3.21 編譯，alpine:3.21 執行，包含 tzdata 和 ca-certificates。
