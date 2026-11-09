# chatbot-go

LINE Bot 生活小幫手：查台灣各地天氣、接收災害警報推播，並能記錄飲食與運動、查看熱量統計、用 AI 回答日常問題。以 Go 開發。

## 功能

- **天氣查詢** -- 輸入區域名稱、簡稱或地標，或直接傳送位置，回傳當地天氣與一句生活建議
- **災害警報推播** -- 天氣特報、地震速報、海嘯警報，依使用者訂閱推播
- **飲食紀錄** -- 拍照或打字記錄餐點，AI 估算熱量與營養素，可事後修改份量
- **運動與熱量** -- 打字或傳 App 截圖記錄運動，計算熱量消耗與每日、每週統計
- **LIFF 統計頁** -- 在 LINE 裡查看熱量圓環、趨勢圖與每日明細
- **AI 問答** -- 直接提問，回答會參考使用者自己的飲食與運動資料
- **圖片選單** -- 聊天室下方六格選單，快速進入各功能

資料僅供參考，AI 估算與建議不構成醫療建議。使用者可隨時輸入「刪除我的資料」清除個人資料。

## 技術架構

Go、Echo、MongoDB、Redis（選用，不可用時自動降級）、LINE Messaging API、中央氣象署開放資料，AI 由多家供應商自動切換。

## 開發

### 前置需求

- Go（版本見 `go.mod`）
- [Task](https://taskfile.dev)
- Docker（整合測試與建置映像用）
- MongoDB（本機執行與整合測試需要）

### 本機執行

```bash
cd build && task run
```

### 程式碼品質

```bash
cd build && task check              # vet、lint、單元測試
cd build && task check:integration  # 整合測試（需要 MongoDB）
cd build && task check:all          # 以上全部
```

### 圖片選單

```bash
cd build && task richmenu           # 建立並設為預設選單（需要 LINE_CHANNEL_TOKEN）
```

## 環境設定

正式環境透過環境變數注入，請勿把機敏值寫進設定檔或提交到版本庫。

| 變數 | 說明 |
|------|------|
| `MONGO_URI` | MongoDB 連線 |
| `REDIS_ADDR` | Redis 位址（選用） |
| `REDIS_PASSWORD` | Redis 密碼（選用） |
| `LINE_CHANNEL_SECRET` | LINE Messaging API channel secret |
| `LINE_CHANNEL_TOKEN` | LINE Messaging API channel token |
| `LINE_LOGIN_CHANNEL_ID` | LIFF 所屬 LINE Login channel ID |
| `LIFF_ID` | LIFF App ID |
| `CWA_AUTH_KEY` | 中央氣象署開放資料授權碼 |
| `GEMINI_API_KEY` | Gemini 金鑰 |
| `GROQ_API_KEY` | Groq 金鑰 |
| `NVIDIA_API_KEY` | NVIDIA 金鑰 |
| `ANTHROPIC_API_KEY` | Claude 金鑰（僅用於 AI 問答） |
| `CLAUDE_MODEL` | AI 問答使用的 Claude 模型 |
| `AI_PROVIDER_ORDER` | 文字類 AI 供應商順序 |
| `AI_VISION_PROVIDER_ORDER` | 圖片類 AI 供應商順序 |
| `AI_QA_PROVIDER_ORDER` | AI 問答供應商順序 |
| `TRUSTED_PROXIES` | 受信任的反向代理設定 |
| `CRYPTO_KEYSET` | 機敏設定的加密金鑰組 |
| `ADMIN_TOKEN` | 管理用權杖 |

## 部署

以 `build/Dockerfile` 建置映像（`cd build && task build`，會先跑完所有檢查），或使用 `build/docker-compose.yml` 啟動，並於部署平台設定上述環境變數。
