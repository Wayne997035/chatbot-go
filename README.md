# chatbot-go

LINE Bot 生活小幫手：查台灣各地天氣、接收災害警報推播，並能記錄飲食與運動、查看熱量統計、用 AI 回答日常問題。以 Go 開發。

## 功能

- **天氣查詢** -- 輸入區域名稱、簡稱或地標，或直接傳送位置，回傳當地天氣與一句生活建議
- **災害警報推播** -- 天氣特報、地震報告、海嘯警報，依使用者訂閱推播
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
| `NVIDIA_MODELS` | NVIDIA 模型清單（逗號分隔、保留大小寫、依序嘗試）；留空 = 自動排名；不合格項目丟棄並記 warn（不含內容） |
| `GROQ_MODELS` | Groq 模型清單，格式與留空意義同 `NVIDIA_MODELS` |
| `GEMINI_MODELS` | Gemini 模型清單，格式與留空意義同 `NVIDIA_MODELS` |
| `AI_ATTEMPT_TIMEOUT_SECONDS` | 單次模型呼叫上限，單位秒（1-3600）；留空 = 程式預設；0 或非法值無效並記 warn；看圖請求至少 15 秒，設得更低對看圖無效 |
| `AI_PROVIDER_BUDGET_SECONDS` | 每家供應商每次請求的時間上限，單位秒（1-3600）；留空 = 程式預設；0 或非法值無效；看圖請求至少 15 秒，設得更低對看圖無效 |
| `AI_PROVIDER_COOLDOWN_SECONDS` | 供應商逾時／連不上／5xx 後排到最後的時間，單位秒（1-3600）；留空 = 程式預設；0 或非法值無效 |
| `AI_RATE_LIMIT_WAIT_SECONDS` | 遇限流願意原地等待的上限，單位秒（0-3600）；留空 = 程式預設；`0` = 不等、直接換模型；要恢復原地等待請明確設 `8` |
<!-- [F33-12] 七個 AI 路由變數；預設值只在 internal/ai 定義，這裡不重複 -->
| `TRUSTED_PROXIES` | 受信任的反向代理設定 |
| `CRYPTO_KEYSET` | 機敏設定的加密金鑰組 |
| `ADMIN_TOKEN` | 管理用權杖 |

<!-- [F34-50] 機敏設定加密段（cmd/secrets 的 user 操作手冊） -->

## 機敏設定加密

**威脅模型**：11 個機敏欄位在 Render 上只放密文 `ENC(v1:…)`，只有 `CRYPTO_KEYSET`（金鑰）是明文。防的是單一值、日誌、截圖、錯貼這類**部分外洩**；金鑰與密文在同一份 Render 環境變數裡，**整份 Render 環境變數外洩**（帳號或 API key 被盜、程式被入侵印出環境）時金鑰一起外洩、全部解得開，這時必須輪替 11 個原值與 keyset。

11 個欄位（對應 `internal/platform/config/secrets.go` 的 `SecretFields`）：`MONGO_URI`、`REDIS_ADDR`、`REDIS_PASSWORD`、`LINE_CHANNEL_SECRET`、`LINE_CHANNEL_TOKEN`、`CWA_AUTH_KEY`、`GEMINI_API_KEY`、`GROQ_API_KEY`、`NVIDIA_API_KEY`、`ANTHROPIC_API_KEY`、`ADMIN_TOKEN`。每個欄位的密文綁定自己的設定路徑，貼錯欄位會解不開。

**keyset 備份警語**：keyset 遺失 = 所有 `ENC` 值無法還原。產生後先貼進密碼管理器備份，再貼進 Render。

執行前關閉 Universal Clipboard（系統設定 > 一般 > AirDrop 與接力 > 接力）並暫停剪貼簿歷史工具：`pbcopy` 寫進的 keyset 與你複製的明文會被它們存下來或同步到其他裝置，程式結束時的清除只清得到目前那一格。跑完確認歷史裡沒有 keyset 與明文。

全部指令都在你自己的 macOS 終端機、從 repo 根目錄執行（keyset 走剪貼簿 `pbcopy`/`pbpaste`，明文走不回顯的隱藏輸入）：

```bash
cd build && task secrets:keyset-new
cd build && task secrets:encrypt
cd build && task secrets:decrypt -- LINE_CHANNEL_TOKEN
cd build && task secrets:keyset-rotate
cd build && task secrets:reencrypt
cd build && task secrets:keyset-status
cd build && task secrets:keyset-prune
```

### 首次上線八步

1. 部署前驗 keyset（Render 已設 `CRYPTO_KEYSET` 才需要）：新版檢查 keyset 比舊版嚴，不合格會讓新版啟動失敗。先確認密碼管理器裡那份與 Render 上的 `CRYPTO_KEYSET` 完全相同，再從密碼管理器複製它，跑 `task secrets:keyset-status`，不貼任何行直接按 Ctrl-D；每把金鑰印一行 `key <id> primary=… used=0`、其中一行 `primary=true` 才算通過。印 `clipboard does not hold a valid keyset` 時：Render 上若有任何 `ENC(` 開頭的值，先全部換回密碼管理器裡的明文並部署一次（舊值綁舊 keyset，換 keyset 後新舊版都解不開），再用 `task secrets:keyset-new` 重產並更新 Render，通過前不要部署。
2. 先部署：警告模式，啟動日誌出現下列各則屬預期 —— keyset 未設時 `config keyset is empty`；有欄位還是明文時 `config has plaintext secrets`（只列欄位名）；沒設 `REQUIRE_ENCRYPTED_SECRETS` 時 `config references unset environment variables`（names 含它）。
3. 產金鑰（第 1 步已有合格 keyset 就跳過）：`task secrets:keyset-new`，keyset 在剪貼簿；貼進密碼管理器與 Render `CRYPTO_KEYSET`，再按 Enter 清剪貼簿。
4. 加密：複製密碼管理器裡的 keyset，`task secrets:encrypt`，逐項輸入 11 個值（空 = 略過，單欄用 `task secrets:encrypt -- <ENV_NAME>`），複製印出的 `NAME=ENC(v1:…)` 行。
5. 在 Render 一次儲存：`CRYPTO_KEYSET` 與要換的 `NAME` 同時改（單次部署）。值欄只貼等號右邊的 `ENC(v1:…)`，不含 `NAME=` 與引號（貼錯會啟動失敗）。有 ENC 值但 keyset 空會啟動失敗，所以兩者必須同一次儲存；明文原值留在密碼管理器當回滾。要把程式碼回滾到本版之前的版本，先把這些值全部換回明文再回滾：其他欄位漏換時舊版會啟動失敗，但 `ADMIN_TOKEN` 例外 —— 舊版不解它，會把整串 `ENC(…)` 當管理 token 照常啟動，所以一定要換。
6. 驗證：部署成功、LINE 照常回應；`config has plaintext secrets` 只剩你刻意留明文的欄位。
7. 全部換完後設 `REQUIRE_ENCRYPTED_SECRETS=true` 再部署；仍正常啟動即完成（有任何欄位刻意留明文就不要開）。回滾：刪除該變數（回警告模式）。
8. 之後要輪替，照下面六步。

### 輪替六步（每步都可回退）

名詞：K1 = 目前部署的 keyset；K2 = K1 加一把新主金鑰（舊鑰保留）；K3 = K2 移除舊鑰。

1. 複製 K1，`task secrets:keyset-rotate` → 剪貼簿是 K2；貼進密碼管理器（K1 保留）。未部署前不影響線上。
2. Render `CRYPTO_KEYSET` 改成 K2 並部署；日誌會出現 `config secrets not on primary key`（預期）。回退：改回 K1。
3. 複製 K2，`task secrets:reencrypt`，貼上 Render 目前的 `NAME=ENC(…)` 行，按 Ctrl-D；複製輸出。任一行錯就整批不輸出，Render 值不變。
4. 把新的 `NAME=ENC(…)` 設進 Render 並部署；複製 K2，`task secrets:keyset-status`，貼上各行，確認全部 env 都在主金鑰、舊金鑰 `used=0`。
5. 複製 K2，`task secrets:keyset-prune`，貼 11 行（未設的寫 `NAME=`），按 Ctrl-D；只有沒有任何一行依賴舊鑰、且 K3 解得開每一行才會通過，K3 在剪貼簿，貼進密碼管理器（K2 保留）。可指定單把：`task secrets:keyset-prune -- --key <id>`。被拒時剪貼簿不動。
6. Render `CRYPTO_KEYSET` 改成 K3 並部署；日誌無 `config secrets not on primary key`、無解密錯誤。回退：改回 K2。驗證一段時間後才從密碼管理器刪 K1、K2。

### decrypt 只在終端機

`task secrets:decrypt -- <ENV_NAME>` 會把該欄原值完整印在螢幕上，用來檢查 ENC 值對不對；先複製 keyset，再貼上該欄的 `ENC(…)`（或 `NAME=ENC(…)`）一行。stdout 不是真終端機（導向檔案、管線、自動化工具）一律拒絕，不解密、不印任何東西。

### richmenu 要自備明文 token

`task richmenu` 需要明文的 `LINE_CHANNEL_TOKEN`（`cmd/richmenu` 不經過設定檔解密）；請從密碼管理器取原值，只在該次指令的環境提供，不要寫進檔案。

### 日後擴充

目前**不採用**任何要額外花錢的服務，維持單層 keyset 放 Render 環境變數。日後若要把 keyset 改由雲端 KMS 包起來（值格式 `KMS(<…>)`；程式已保留這個前綴並一律拒絕啟動，且測試證明 keyset 包上 KMS 後 11 個 ENC 值免重加密）：

- 需求與成本：KMS 需雲端帳號與 IAM（GCP 與 AWS 都按金鑰與呼叫次數計費）；免存雲端金鑰要用 Render 的 OIDC，官方要求 Pro 工作區；Hobby 工作區只能把服務帳號金鑰放環境變數，那把金鑰同樣是機密。
- 轉換條件：要加密 DB 個資欄位、多服務共用同一 keyset、換雲，或需要金鑰獨立撤銷與審計；屆時另開單。

## 部署

以 `build/Dockerfile` 建置映像（`cd build && task build`，會先跑完所有檢查），或使用 `build/docker-compose.yml` 啟動，並於部署平台設定上述環境變數。
