# chatbot-go

LINE Bot 天氣查詢機器人，以 Go 開發，串接中央氣象署 CWA Open Data API，提供台灣各縣市鄉鎮區的即時天氣預報查詢，並支援災害警報推播通知。

## 功能

### 天氣查詢
- **文字查詢** -- 使用者輸入區域名稱（如「信義區」），回傳該區域天氣預報
- **模糊查詢** -- 支援簡稱、地標名稱（如「北車」、「天母」、「日月潭」），系統自動比對對應行政區；若有多個候選地點，進入多輪對話請使用者選擇
- **Geocoding fallback** -- 模糊比對找不到時，透過 OpenStreetMap Nominatim 解析地名（1 req/s rate limit，結果 Redis cache 24h）
- **位置查詢** -- 使用者傳送 LINE 位置訊息，自動解析地址並回傳當地天氣
- **排程同步** -- 透過 cron 定時從 CWA Open Data API 抓取全台 22 縣市天氣資料
- **手動觸發** -- 提供 API endpoint 手動觸發天氣資料同步
- **Redis 快取** -- cache-aside pattern，減少 MongoDB 查詢負擔，TTL 1 小時
- **AI 生活建議** -- 天氣回覆附一句穿著、帶傘、防曬建議；AI 不可用時改用規則判斷，天氣回覆不受影響
- **快速回覆按鈕** -- 回覆下方附「傳送我的位置」等按鈕，不用打字

### 災害警報推播
- **天氣特報** -- 當 CWA 發布大雨/豪雨特報時，推播通知給訂閱該地區的使用者
- **地震速報** -- 地震報告發布時，推播通知給有開啟地震警告的使用者
- **海嘯警報** -- 海嘯警報發布時（非綠色解除），推播通知給有開啟地震海嘯警告的使用者
- **排程檢查** -- 每 5 分鐘檢查 CWA API，Redis dedup 避免重複推播
- **訂閱管理** -- 使用者透過 LINE 關鍵字或 REST API 訂閱/取消

### 飲食紀錄
- **拍照分析營養** -- 傳送餐點照片，AI 辨識每樣食物並估算份量、熱量、蛋白質、碳水、脂肪，附一句飲食建議
- **自動記錄** -- 每張照片存成一餐，回覆附今日累計熱量；不保存照片本身，只存分析結果
- **今日飲食** -- 輸入「今日飲食」看今天每一餐與合計；認錯時輸入「刪除上一筆」
- **用量保護** -- 每人每天最多分析 30 張照片，避免免費 AI 額度被用完
- 數值為 AI 估算，僅供參考

### 運動與熱量統計
- **打字記錄（寬鬆）** -- 「跑步 5公里 30分鐘」「跑5k 30分」「今天慢跑半小時」「重訓一個半小時」都看得懂；只點「跑步」會再用按鈕問時間
- **AI 補位** -- 規則看不懂但像在講運動的句子交給 AI 解析，先請使用者確認才存
- **運動截圖** -- 傳 Apple Health、Garmin 等 App 截圖，AI 讀出種類、時間、距離與熱量
- **熱量計算** -- MET × 體重 × 時間；跑步、快走、騎車依速度調整強度；截圖有熱量時直接採用
- **身體資料** -- 「體重 70 身高 170 30歲 男」設定，計算基礎代謝（Mifflin-St Jeor）
- **統計** -- 「今日統計」看攝取、運動與熱量赤字；「本週統計」看近 7 天
- **資料刪除** -- 「刪除上一筆運動」；「刪除我的資料」確認後刪除所有飲食、運動與身體資料

### 圖片選單（Rich Menu）
- 聊天室下方六格選單：查天氣、拍食物、記運動、今日統計、本週統計、說明
- 建立或更新：`LINE_CHANNEL_TOKEN=... go run ./cmd/richmenu`（或 `task richmenu`），會設為所有人的預設選單並移除舊版

### AI 串接（`internal/ai`）
- **三家自動切換** -- Gemini、Groq、NVIDIA，文字與看圖各有順序；一家內逐一嘗試候選模型，整家失敗才換下一家
- **模型不寫死** -- 向供應商列出可用模型後依通用規則排序，舊模型下架不必改程式
- **回答檢查** -- 回答必須通過格式檢查（例如可解析的 JSON），不合格視同失敗繼續切換
- **限流處理** -- 短暫限流先等再重試同模型；請求超過上限、每日額度用完或模型不存在則冷卻一段時間跳過
- **時間預算** -- 每家有時間上限，單次呼叫逾時不超過剩餘預算，確保在 LINE 回覆期限內回應
- **錯誤遮蔽** -- 錯誤訊息先遮蔽帳號 ID 與金鑰才寫進 log，不會回覆給使用者

### 其他
- **機敏資料保護** -- 機敏設定採加密保護，正式環境透過環境變數注入。

## 技術架構

| 項目 | 技術 |
|------|------|
| 語言 | Go 1.26.2 |
| HTTP 框架 | Echo v4 |
| 資料庫 | MongoDB (mongo-driver v2) |
| 快取 | Redis (go-redis v9) |
| 排程 | gocron v2 |
| 日誌 | log/slog + lumberjack |
| 設定 | YAML + 環境變數替換 |
| 容器 | Docker multi-stage build (Alpine)，非 root 執行 |
| Lint | golangci-lint v2 (23 linters) |
| CI/CD | GitHub Actions |

## 專案結構

```
cmd/api/main.go                          # 進入點
configs/                                 # 設定檔與環境變數
internal/
  platform/                              # 基礎設施層 (config, logger, driver, server...)
  storage/database/                      # 資料存取層 (repositories)
  ai/                                    # AI 供應商自動選模與故障切換
  nutrition/                             # 食物照片營養分析
  fitness/                               # 運動解析、熱量計算與統計
  richmenu/                              # 圖片選單定義與圖片
cmd/richmenu/                            # 建立圖片選單的指令
  webhook/                               # LINE webhook 處理
  alert/                                 # 災害警報模組
  conversation/                          # 對話狀態管理
  geocoding/                             # Nominatim geocoding（rate limit + Redis cache）
  weather/                               # 天氣業務邏輯
  user/                                  # 使用者管理
  crypto/                                # 機敏設定加密保護
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

## 設定參數

透過環境變數注入（`configs/production.yaml`），值可用 `ENC(...)` 加密。

| 名稱 | 用途 |
|------|------|
| `MONGO_URI` / `REDIS_ADDR` / `REDIS_PASSWORD` | 資料庫與快取 |
| `LINE_CHANNEL_SECRET` / `LINE_CHANNEL_TOKEN` | LINE Messaging API |
| `CWA_AUTH_KEY` | 中央氣象署 Open Data |
| `CRYPTO_KEYSET` | 設定加密金鑰 |
| `ADMIN_TOKEN` | 管理 API 的驗證 token（正式環境必填，至少 32 字元） |
| `GEMINI_API_KEY` / `GROQ_API_KEY` / `NVIDIA_API_KEY` | AI 供應商，都沒設時 AI 功能停用 |
| `AI_PROVIDER_ORDER` / `AI_VISION_PROVIDER_ORDER` | 文字 / 看圖請求的供應商順序（逗號分隔），留空用預設 |

NVIDIA 免費 API Catalog 依試用條款僅限開發與評估用途。

除 `/health` 與 LINE webhook 外，REST 管理 API 需要 `X-Admin-Token` 或 `Authorization: Bearer <ADMIN_TOKEN>`。

## LINE Bot 操作說明

輸入「說明」可查看所有功能。

### 天氣查詢

直接輸入區域名稱即可查詢天氣，支援精確、模糊、地標三種方式：

| 輸入 | 比對方式 | 回覆 |
|------|----------|------|
| `信義區` | 精確比對 | 信義區天氣預報 |
| `大安` | Fuzzy 比對 | 台北市大安區天氣預報 |
| `北車` | Geocoding | 台北市中正區天氣預報 |
| `天母` | Fuzzy / Geocoding | 台北市士林區天氣預報 |
| `日月潭` | Geocoding | 南投縣魚池鄉天氣預報 |
| 傳送位置訊息 | 地址解析 | 自動回傳當地天氣 |

若有多個符合地點（如「中山」同時符合多縣市），Bot 會以按鈕卡片列出選項，點選即可查詢，無須輸入數字。

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
|--------|------|------|
| 天氣警特報 | 各縣市目前天氣警特報狀態 | 每 5 分鐘 |
| 地震報告 | 有感地震報告（limit=1 取最新） | 每 5 分鐘 |
| 海嘯警報 | 海嘯資訊（過濾綠色=解除） | 每 5 分鐘 |

CWA API 授權金鑰需至 [CWA Open Data 平台](https://opendata.cwa.gov.tw) 註冊取得。

## 開發

### 前置需求

- Go 1.26.2+
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

Docker image 使用 Go 1.26.2 Alpine builder，runtime 以非 root 使用者執行，並內建 `GET /health` healthcheck。
`.dockerignore` 會排除 `.git`、本機設定、secret、log 與 build/test 產物；容器部署請使用 `APP_ENV=production` 與環境變數注入設定。

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
| `CRYPTO_KEYSET` | 機敏設定加密金鑰 |
| `ADMIN_TOKEN` | REST 管理 API token，production 需至少 32 字元 |

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
build (Docker image, only on main branch)
```

測試全部通過才會進行 build。build 失敗不會產出 image。

## 部署

### Docker Compose

```bash
cd build && docker compose up -d
```

本機 compose 會以唯讀 volume 掛載 `configs/local.yaml`，避免本機 secret 被打包進 Docker image。
