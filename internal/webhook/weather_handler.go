package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/geocoding"
	"chatbot-go/internal/models"
	"chatbot-go/internal/redact"
	weatherstore "chatbot-go/internal/storage/database/weather"
	"chatbot-go/internal/weather"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// maxButtonLabelLen LINE Flex 按鈕 label 的字數上限；超過時整則回覆會被 LINE 以 400 拒收
// （正式環境 log 2026-11-03 21:22:12「label must not be longer than 40 characters」）.
const maxButtonLabelLen = 40

// maxQuickReplyLabelLen LINE 快速回覆 label 的字數上限；超過時整則回覆會被拒收.
const maxQuickReplyLabelLen = 20

// buttonLabel [F24-03][F24-07] 把按鈕文字截到 LINE 上限內，被截斷時結尾加「…」.
// LINE 以字素（grapheme cluster）計 label 字數（https://developers.line.biz/en/docs/messaging-api/text-character-count/）；
// 這裡刻意改用 UTF-16 長度計算（emoji 算 2），比 LINE 的算法嚴格，只會截得更保守、不會超限.
// 以字素單元為單位截斷，不會切出半個國旗、孤立的 ZWJ / variation selector / 膚色.
func buttonLabel(s string) string {
	return truncateLabel(s, maxButtonLabelLen)
}

// truncateLabel [F24-87] 把 s 以 UTF-16 長度截到 limit 內，被截斷時結尾加「…」；地點卡片與快速回覆共用這一份.
//
// [F37-05] 兩個已知邊界：
//   - 第一個字素單元就超過 limit-1（例如 47 個 UTF-16 單元的 ZWJ 序列）時結果只有「…」：
//     自己的字素切分只是 LINE 切法的近似，切進一個序列中間可能產生半個 ZWJ 序列或國旗；
//     實際來源（Nominatim / CWA 地名）不會以 40 單元的單一字素開頭，按鈕點下去送出的文字另由 City+District 決定，所以接受只剩「…」.
//   - 非法 UTF-8：先以 strings.ToValidUTF8 把連續壞位元組收成一個 U+FFFD，輸出永遠是合法 UTF-8，單元數不會比原本多.
func truncateLabel(s string, limit int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if utf16Len(s) <= limit {
		return s
	}
	var b strings.Builder
	n := 0
	for _, unit := range graphemeUnits(s) {
		l := utf16Len(unit)
		if n+l > limit-1 {
			break
		}
		b.WriteString(unit)
		n += l
	}
	b.WriteString("…")
	return b.String()
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

func isRegionalIndicator(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

// isClusterExtender 跟著前一個字走的修飾字元：variation selector、膚色、keycap、tag.
func isClusterExtender(r rune) bool {
	return (r >= 0xFE00 && r <= 0xFE0F) || (r >= 0x1F3FB && r <= 0x1F3FF) || r == 0x20E3 || (r >= 0xE0020 && r <= 0xE007F)
}

// graphemeUnits 把字串切成不可分割的單元：成對的國旗、base + 修飾字元、以 ZWJ 連起來的序列.
func graphemeUnits(s string) []string {
	rs := []rune(s)
	var units []string
	for i := 0; i < len(rs); {
		end := unitEnd(rs, i)
		units = append(units, string(rs[i:end]))
		i = end
	}
	return units
}

// unitEnd 回傳從 i 開始的字素單元結束位置（不含）.
func unitEnd(rs []rune, i int) int {
	if isRegionalIndicator(rs[i]) && i+1 < len(rs) && isRegionalIndicator(rs[i+1]) {
		i += 2
	} else {
		i++
	}
	for i < len(rs) {
		switch {
		case isClusterExtender(rs[i]):
			i++
		case rs[i] == 0x200D: // ZWJ 連同其後一個字元
			i++
			if i < len(rs) {
				i++
			}
		default:
			return i
		}
	}
	return i
}

const (
	// barePlaceDailyLimit [F38-07] 單打短文字每人每天送地名服務的次數：修改前同一批訊息要先過意圖 AI（intentDailyLimit 60），維持同等上限.
	barePlaceDailyLimit = 60
	// barePlaceGeocodeTimeout [F38-07] 單打短文字等地名服務（含排全服務共用的 1 req/s 限速）的上限；實測單次 216–793ms.
	barePlaceGeocodeTimeout = 2 * time.Second
)

// geocodeMode [F38-02] replyWeatherQuery 對地名服務（geocoding）的用法；一個參數三種意思，不用兩個 bool.
type geocodeMode int

const (
	// geocodeOff 只走精確與模糊（官方縣市鄉鎮名），不送地名服務.
	geocodeOff geocodeMode = iota
	// geocodeExtracted 意圖 AI 抽出的地名（「陽明山天氣」→「陽明山」）：唯一且非公車站結果直接回天氣.
	geocodeExtracted
	// geocodeBareText 使用者單打的短文字（天母、陽明山）：只留區域或景點類結果，一律出選項卡.
	geocodeBareText
)

// replyWeatherQuery 依序用精確、模糊、geocoding 比對地名並回覆天氣；回傳 true 表示已回覆.
// [F30-66] 打字原文預設只走精確與模糊；[F38-02] stepWeather 對 2–8 字、不含意圖提示字的單打短文字傳 geocodeBareText
// （只認區域或景點類結果，「平安」「幸福」這類一般詞只回車站、站牌，不會出卡）；意圖 AI 抽出的地名（stepIntent）傳 geocodeExtracted.
func (h *WebhookHandler) replyWeatherQuery(ctx context.Context, userID, text, replyToken string, mode geocodeMode) bool {
	// 精確比對
	forecasts, err := h.weatherLookup.ByDistrict(ctx, text)
	if err != nil {
		// [F24-95] 不記使用者輸入原文與位置，只記字數.
		slog.Error("weather lookup by district", "query_len", utf8.RuneCountInString(text), "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "查詢天氣資料時發生錯誤，請稍後再試")
		return true
	}

	if len(forecasts) == 1 {
		h.replyForecast(ctx, userID, replyToken, &forecasts[0], "")
		return true
	}
	if len(forecasts) > 1 {
		_ = h.line.ReplyText(ctx, replyToken, weather.FormatForecasts(forecasts), weatherQuickReplies()...)
		return true
	}

	// Fuzzy 比對
	if h.weatherRepo != nil {
		if done := h.handleFuzzyMatch(ctx, userID, text, replyToken); done {
			return true
		}
	}

	// Geocoding fallback
	if h.geocoder != nil {
		switch mode {
		case geocodeExtracted:
			return h.handleGeocodingFallback(ctx, userID, text, replyToken)
		case geocodeBareText:
			return h.handleBarePlaceGeocode(ctx, userID, text, replyToken)
		case geocodeOff:
		}
	}
	return false
}

// maxCandidateButtons [F24-89] 候選卡片按鈕數上限：鄉鎮最多的縣市高雄市有 38 個，
// 且 LINE bubble JSON 上限 30 KB.
const maxCandidateButtons = 40

// handleFuzzyMatch 執行模糊比對，回傳 true 表示已處理（不需繼續往下）.
func (h *WebhookHandler) handleFuzzyMatch(ctx context.Context, userID, text, replyToken string) bool {
	candidates, err := h.weatherRepo.FindAllDistricts(ctx)
	if err != nil {
		slog.Error("find all districts", "error", err)
		return false
	}

	matches := weather.FuzzyMatch(candidates, text)

	// [F24-89] 候選太多不出卡片，請使用者打完整地名；不設對話狀態.
	if len(matches) > maxCandidateButtons {
		_ = h.line.ReplyText(ctx, replyToken,
			fmt.Sprintf("符合的地點有 %d 個，請打得更完整，例如「台北市大安區」", len(matches)))
		return true
	}

	switch len(matches) {
	case 0:
		return false

	case 1:
		forecast, err := h.weatherLookup.ByCityAndDistrict(ctx, matches[0].City, matches[0].District)
		if err != nil {
			slog.Error("weather lookup by city and district (fuzzy)", "error", err)
			_ = h.line.ReplyText(ctx, replyToken, "查詢天氣資料時發生錯誤，請稍後再試")
			return true
		}
		if forecast == nil {
			return false
		}
		h.replyForecast(ctx, userID, replyToken, forecast, "")
		return true

	default:
		// 多個結果 → 存 TypeLocationClarify state，回覆候選清單
		convCandidates := make([]conversation.CandidateLocation, 0, len(matches))
		for _, m := range matches {
			convCandidates = append(convCandidates, conversation.CandidateLocation{
				City:        m.City,
				District:    m.District,
				DisplayName: m.City + m.District,
				Source:      "fuzzy",
			})
		}

		clarifyState := &conversation.State{
			Type:       conversation.TypeLocationClarify,
			Candidates: convCandidates,
		}
		err := h.convManager.Set(ctx, userID, clarifyState)
		// [F291-03] 候選地名由使用者文字查得；刪除後落地的候選狀態要清掉，不送候選卡片.
		if h.guardAfterWrite(ctx, userID, replyToken, h.undoState(userID)) {
			return true
		}
		if err != nil {
			slog.Error("set location clarify state", "error", redact.Error(err))
		}

		_ = h.line.ReplyFlex(ctx, replyToken, "找到多個符合的地點，請選擇", buildCandidateFlexMessage(convCandidates))
		return true
	}
}

// handleGeocodingFallback 執行 geocoding 查詢，回傳 true 表示已處理.
func (h *WebhookHandler) handleGeocodingFallback(ctx context.Context, userID, text, replyToken string) bool {
	results, err := h.geocoder.Geocode(ctx, text)
	if err != nil {
		// [F38-06] 錯誤可能夾帶請求 URL（q= 是原文）：過 redact.Error 才記.
		slog.Error("geocode query failed", "query_len", len([]rune(text)), "error", redact.Error(err))
		return false
	}

	switch {
	case len(results) == 0:
		return false

	// [F241-14] 唯一結果且只來自公車站（NeedsConfirm）時不直接回天氣，走 default 的確認卡片.
	case len(results) == 1 && !results[0].NeedsConfirm:
		forecast, err := h.weatherLookup.ByCityAndDistrict(ctx, results[0].City, results[0].District)
		if err != nil {
			slog.Error("weather lookup by city and district (geocode)", "error", err)
			_ = h.line.ReplyText(ctx, replyToken, "查詢天氣資料時發生錯誤，請稍後再試")
			return true
		}
		if forecast == nil {
			_ = h.line.ReplyText(ctx, replyToken, "抱歉找不到該地點的天氣資料")
			return true
		}
		h.replyForecast(ctx, userID, replyToken, forecast, osmAttribution)
		return true

	default:
		h.replyGeocodeCandidates(ctx, userID, replyToken, results, "") // 意圖路徑的卡不加「不是要查天氣」
		return true
	}
}

// handleBarePlaceGeocode [F38-02][F38-09] 單打短文字的地名服務查詢：有任何結果就一律出選項卡（含只有 1 筆）；
// 有 Area（區域或景點類）結果時卡上只列 Area（陽明山不列公車站），沒有時列全部結果；卡的最下面多一顆「不是要查天氣」.
// 0 筆或地名服務出錯回 false，後面的步驟照舊. 與 handleGeocodingFallback 分開：後者唯一結果直接回天氣、不看 Area.
// [F38-07] 這條路是「猜它可能是地名」：每人每天最多 barePlaceDailyLimit 次、每次最多等 barePlaceGeocodeTimeout，
// 超過都照舊往下 —— 一個人連發短訊息不能佔滿全服務共用的 Nominatim 限速（1 req/s）與事件 worker.
func (h *WebhookHandler) handleBarePlaceGeocode(ctx context.Context, userID, text, replyToken string) bool {
	if !h.withinDailyLimit(ctx, "geocode:count", userID, barePlaceDailyLimit) {
		return false
	}
	gctx, cancel := context.WithTimeout(ctx, barePlaceGeocodeTimeout)
	defer cancel()
	results, err := h.geocoder.Geocode(gctx, text)
	if err != nil {
		// [F24-95][F38-06] 只記字數，錯誤過 redact.Error（可能夾帶 q=原文 的 URL）；出錯或逾時照舊往下，不回錯誤句.
		slog.Error("geocode bare place failed", "query_len", utf8.RuneCountInString(text), "error", redact.Error(err))
		return false
	}
	areas := make([]geocoding.GeocodeResult, 0, len(results))
	for _, r := range results {
		if r.Area {
			areas = append(areas, r)
		}
	}
	if len(areas) == 0 {
		areas = results
	}
	if len(areas) == 0 {
		return false
	}
	h.replyGeocodeCandidates(ctx, userID, replyToken, areas, text)
	return true
}

// replyGeocodeCandidates 把地名服務的候選存成 TypeLocationClarify 狀態並回候選選項卡（handleGeocodingFallback 與 handleBarePlaceGeocode 共用）.
// [F38-09] notWeatherQuery 非空（單打路徑傳使用者原文）時，卡片最下面多一顆「不是要查天氣」；空字串 = 不加（意圖路徑）.
// 這顆按鈕不是地點，不進對話狀態的候選清單.
func (h *WebhookHandler) replyGeocodeCandidates(ctx context.Context, userID, replyToken string, results []geocoding.GeocodeResult, notWeatherQuery string) {
	convCandidates := make([]conversation.CandidateLocation, 0, len(results))
	for _, r := range results {
		displayName := r.DisplayName
		if displayName == "" {
			displayName = r.City + r.District
		}
		convCandidates = append(convCandidates, conversation.CandidateLocation{
			City:        r.City,
			District:    r.District,
			DisplayName: displayName,
			Source:      "geocode",
		})
	}

	clarifyState := &conversation.State{
		Type:       conversation.TypeLocationClarify,
		Candidates: convCandidates,
	}
	err := h.convManager.Set(ctx, userID, clarifyState)
	// [F291-03] 同上（geocode 候選）.
	if h.guardAfterWrite(ctx, userID, replyToken, h.undoState(userID)) {
		return
	}
	if err != nil {
		slog.Error("set location clarify state (geocode)", "error", redact.Error(err))
	}

	altText := "找到多個符合的地點，請選擇"
	if len(convCandidates) == 1 {
		altText = candidateConfirmAltText
	}
	card := buildCandidateFlexMessage(convCandidates)
	if notWeatherQuery != "" {
		addNotWeatherButton(card, notWeatherQuery)
	}
	_ = h.line.ReplyFlex(ctx, replyToken, altText, card)
}

// notWeatherLabel [F38-09] 單打地名卡最下面的退路按鈕：點了送「問 <原文>」走既有問答.
const notWeatherLabel = "不是要查天氣"

// addNotWeatherButton [F38-09] 在 buildCandidateFlexMessage 的卡片最下面附「不是要查天氣」message 按鈕（送 askPrefixes[0] + 原文）.
func addNotWeatherButton(card *models.BubbleContainer, original string) {
	card.Footer.Contents = append(card.Footer.Contents, models.ButtonComponent{
		Type:   "button",
		Style:  "secondary",
		Height: "sm",
		Action: &models.MessageAction{
			Type:  actionTypeMessage,
			Label: notWeatherLabel,
			Text:  askPrefixes[0] + original,
		},
	})
}

// candidateConfirmAltText [F241-14][F38-02] 只有一個候選（只來自公車站，或單打地名只有一個區域）時的通知文字.
const candidateConfirmAltText = "找到一個符合的地點，請確認"

// buildCandidateFlexMessage [F24-03] 產生候選地點選擇 Flex Message.
// [F241-14] 只有一個候選時標題寫「找到一個符合的地點」，2 個以上維持「找到多個符合的地點」.
func buildCandidateFlexMessage(candidates []conversation.CandidateLocation) *models.BubbleContainer {
	candidateTitle := "找到多個符合的地點"
	if len(candidates) == 1 {
		candidateTitle = "找到一個符合的地點"
	}

	buttons := make([]any, 0, len(candidates))
	for i, c := range candidates {
		style := "secondary"
		if i == 0 {
			style = "primary"
		}
		// [F24-86] 送出「縣市+鄉鎮」讓聊天紀錄看得懂；缺任一則送編號.
		text := strconv.Itoa(i + 1)
		if c.City != "" && c.District != "" {
			text = c.City + c.District
		}
		buttons = append(buttons, models.ButtonComponent{
			Type:   "button",
			Style:  style,
			Height: "sm",
			Action: &models.MessageAction{
				Type:  actionTypeMessage,
				Label: buttonLabel(c.DisplayName),
				Text:  text,
			},
		})
	}

	return &models.BubbleContainer{
		Type: "bubble",
		Body: &models.BoxComponent{
			Type:    "box",
			Layout:  "vertical",
			Spacing: "sm",
			Contents: []any{
				models.TextComponent{
					Type:   "text",
					Text:   candidateTitle,
					Weight: "bold",
					Size:   "md",
				},
				models.TextComponent{
					Type:  "text",
					Text:  "請點選要查詢的地點",
					Size:  "sm",
					Color: "#888888",
				},
			},
		},
		Footer: &models.BoxComponent{
			Type:     "box",
			Layout:   "vertical",
			Spacing:  "sm",
			Contents: buttons,
		},
	}
}

func (h *WebhookHandler) handleLocationMessage(ctx context.Context, userID, address, replyToken string) {
	address = strings.ReplaceAll(address, "臺", "台")
	city, district := ParseAddress(address)
	if city == "" || district == "" {
		_ = h.line.ReplyText(ctx, replyToken, "無法解析地址，請傳送正確的位置資訊")
		return
	}

	forecast, err := h.weatherLookup.ByCityAndDistrict(ctx, city, district)
	if err != nil {
		slog.Error("weather lookup by location", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "查詢天氣資料時發生錯誤，請稍後再試")
		return
	}

	if forecast == nil {
		_ = h.line.ReplyText(ctx, replyToken, "找不到「"+city+district+"」的天氣資料")
		return
	}

	h.replyForecast(ctx, userID, replyToken, forecast, "")
}

// replyForecast 回覆單一地區天氣，附上生活建議與快速回覆按鈕.
func (h *WebhookHandler) replyForecast(
	ctx context.Context, userID, replyToken string, forecast *weatherstore.WeatherForecast, note string,
) {
	// [F24-64] 刪除資料之前送出的查詢不把地點記回去；天氣預報本身是公開資料，照常回覆
	var undoErr error
	if !h.erasedAfterSent(ctx, userID) {
		h.locations.Remember(ctx, userID, forecast.City, forecast.District)
		// [F241-07][F291-05] 寫後再查：刪除流程先 mark 再 Forget，所以檢查與寫入之間完成的刪除，這裡一定看得到標記；
		// 補刪失敗要讓使用者知道（預報照回，最後附一行），不能默默留著已承諾刪除的地點.
		_, undoErr = h.recheckAfterWrite(ctx, userID, func(cctx context.Context) error {
			return h.locations.Forget(cctx, userID)
		})
	}
	reply := weather.FormatSingleForecast(forecast)
	if tip := h.advisor.Tip(ctx, forecast); tip != "" {
		reply += "建議：" + tip + "\n"
	}
	if note != "" {
		reply += note
	}
	reply = strings.TrimRight(reply, "\n")
	if undoErr != nil {
		reply += "\n" + eraseFailedMessage
	}
	_ = h.line.ReplyText(ctx, replyToken, reply, weatherQuickReplies()...)
}

// weatherQuickReplies 天氣回覆下方的快速按鈕.
func weatherQuickReplies() []models.QuickReplyItem {
	return []models.QuickReplyItem{
		models.LocationItem("傳送我的位置"),
		models.MessageItem("開啟天氣特報", keywordWeatherWarningOn),
		models.MessageItem("使用說明", keywordHelp),
	}
}

// clarifyNoSuchOptionMessage [F37-04] 選單開著時打了選不到候選的數字.
const clarifyNoSuchOptionMessage = "沒有這個選項，請點選上方卡片的地點"

// isDigitOnly s 非空且全是 ASCII 0-9 或全形 ０-９.
func isDigitOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < '０' || r > '９') {
			return false
		}
	}
	return true
}

// asciiDigits 把全形數字 ０-９ 轉成半形；呼叫前已確認 s 只含數字.
func asciiDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '０' && r <= '９' {
			return r - '０' + '0'
		}
		return r
	}, s)
}

// hasAskPrefix [F38-09] 文字以「問 」「問:」「問：」開頭（askPrefixes）.
func hasAskPrefix(text string) bool {
	for _, p := range askPrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// handleLocationClarify 處理地點澄清對話（多候選地點時）.
// [F26-04] 回傳 false 表示使用者的字對不上任何候選（換話題了）：狀態已清除、沒有回覆，呼叫端照一般流程處理這則訊息.
func (h *WebhookHandler) handleLocationClarify(
	ctx context.Context, userID, text, replyToken string, state *conversation.State,
) bool {
	candidates := state.Candidates
	if len(candidates) == 0 {
		_ = h.convManager.Delete(ctx, userID)
		_ = h.line.ReplyText(ctx, replyToken, "找不到候選地點，請重新輸入地區名稱")
		return true
	}

	// [F38-09] 「問 …」（含「不是要查天氣」按鈕送的「問 <原文>」）是換話題：不拿去比對候選（原文含區名時會被誤選成天氣）.
	selected := findClarifyCandidate(text, candidates)
	if hasAskPrefix(text) {
		selected = nil
	}
	if selected == nil && isDigitOnly(strings.TrimSpace(text)) {
		// [F37-04] 只含數字、選不到候選（0、超出張數、長到 Atoi 解析不了）：選單保留（不 Delete、不 Set，
		// 到期時間不延長）、不進 AI；卡片按鈕沒有編號，所以不出現「1~N」.
		_ = h.line.ReplyText(ctx, replyToken, clarifyNoSuchOptionMessage)
		return true
	}
	if selected == nil {
		if err := h.convManager.Delete(ctx, userID); err != nil {
			slog.Error("delete location clarify state", "error", err)
		}
		return false
	}

	// 清除對話狀態
	if err := h.convManager.Delete(ctx, userID); err != nil {
		slog.Error("delete location clarify state", "error", err)
	}

	// 查詢天氣
	forecast, err := h.weatherLookup.ByCityAndDistrict(ctx, selected.City, selected.District)
	if err != nil {
		slog.Error("weather lookup after clarify", "error", err)
		_ = h.line.ReplyText(ctx, replyToken, "查詢天氣資料時發生錯誤，請稍後再試")
		return true
	}

	if forecast == nil {
		_ = h.line.ReplyText(ctx, replyToken, "抱歉找不到該地點的天氣資料")
		return true
	}

	note := ""
	if selected.Source == "geocode" {
		note = osmAttribution
	}
	h.replyForecast(ctx, userID, replyToken, forecast, note)
	return true
}

// findClarifyCandidate 從 candidates 中根據 user 輸入找到選擇的候選地點.
// 支援（依序）：縣市+鄉鎮完全相同、數字索引（"1"、"2"...）、包含 district 名稱的文字.
// [F26-04] District 為空的候選不參與文字比對；「候選區名包含使用者打的字」只在使用者打 ≥2 個字時成立.
func findClarifyCandidate(text string, candidates []conversation.CandidateLocation) *conversation.CandidateLocation {
	text = strings.TrimSpace(text)

	// [F24-86] 先精確比對「縣市+鄉鎮」（臺→台 正規化後相等）；同名鄉鎮才不會選錯.
	for i, c := range candidates {
		if c.City != "" && c.District != "" && strings.ReplaceAll(c.City+c.District, "臺", "台") == text {
			result := candidates[i]
			return &result
		}
	}

	// 嘗試數字選擇
	// [F37-04] 全形數字（「２」）只在整串都是數字時轉成半形再交給 Atoi；Atoi 本身接受的範圍（例如「+2」）不變.
	numText := text
	if isDigitOnly(text) {
		numText = asciiDigits(text)
	}
	if idx, err := strconv.Atoi(numText); err == nil {
		if idx >= 1 && idx <= len(candidates) {
			c := candidates[idx-1]
			return &c
		}
		return nil
	}

	// 嘗試地名比對（包含 district）
	for i, c := range candidates {
		if c.District == "" {
			continue
		}
		if strings.Contains(text, c.District) || (utf8.RuneCountInString(text) >= 2 && strings.Contains(c.District, text)) {
			result := candidates[i]
			return &result
		}
	}

	return nil
}
