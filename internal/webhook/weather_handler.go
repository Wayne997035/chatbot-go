package webhook

import (
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/models"
	weatherstore "chatbot-go/internal/storage/database/weather"
	"chatbot-go/internal/weather"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// maxButtonLabelLen LINE Flex 按鈕 label 的字數上限；超過時整則回覆會被 LINE 以 400 拒收
// （正式環境 log 2026-11-03 21:22:12「label must not be longer than 40 characters」）.
const maxButtonLabelLen = 40

// buttonLabel [F24-03][F24-07] 把按鈕文字截到 LINE 上限內，被截斷時結尾加「…」.
// LINE 怎麼數字數（rune 或 UTF-16）未驗證；用 UTF-16 長度計算（emoji 算 2），比數 rune 保守.
// 以字素單元為單位截斷，不會切出半個國旗、孤立的 ZWJ / variation selector / 膚色.
func buttonLabel(s string) string {
	if utf16Len(s) <= maxButtonLabelLen {
		return s
	}
	var b strings.Builder
	n := 0
	for _, unit := range graphemeUnits(s) {
		l := utf16Len(unit)
		if n+l > maxButtonLabelLen-1 {
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

// replyWeatherQuery 依序用精確、模糊、geocoding 比對地名並回覆天氣；回傳 true 表示已回覆.
func (h *WebhookHandler) replyWeatherQuery(ctx context.Context, userID, text, replyToken string) bool {
	// 精確比對
	forecasts, err := h.weatherLookup.ByDistrict(ctx, text)
	if err != nil {
		slog.Error("weather lookup by district", "district", text, "error", err)
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
		if done := h.handleGeocodingFallback(ctx, userID, text, replyToken); done {
			return true
		}
	}
	return false
}

// handleFuzzyMatch 執行模糊比對，回傳 true 表示已處理（不需繼續往下）.
func (h *WebhookHandler) handleFuzzyMatch(ctx context.Context, userID, text, replyToken string) bool {
	candidates, err := h.weatherRepo.FindAllDistricts(ctx)
	if err != nil {
		slog.Error("find all districts", "error", err)
		return false
	}

	matches := weather.FuzzyMatch(candidates, text)

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
		if err := h.convManager.Set(ctx, userID, clarifyState); err != nil {
			slog.Error("set location clarify state", "error", err)
		}

		_ = h.line.ReplyFlex(ctx, replyToken, "找到多個符合的地點，請選擇", buildCandidateFlexMessage(convCandidates))
		return true
	}
}

// handleGeocodingFallback 執行 geocoding 查詢，回傳 true 表示已處理.
func (h *WebhookHandler) handleGeocodingFallback(ctx context.Context, userID, text, replyToken string) bool {
	results, err := h.geocoder.Geocode(ctx, text)
	if err != nil {
		slog.Error("geocode query failed", "query_len", len([]rune(text)), "error", err)
		return false
	}

	switch len(results) {
	case 0:
		return false

	case 1:
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
		if err := h.convManager.Set(ctx, userID, clarifyState); err != nil {
			slog.Error("set location clarify state (geocode)", "error", err)
		}

		_ = h.line.ReplyFlex(ctx, replyToken, "找到多個符合的地點，請選擇", buildCandidateFlexMessage(convCandidates))
		return true
	}
}

// buildCandidateFlexMessage 產生候選地點選擇 Flex Message.
func buildCandidateFlexMessage(candidates []conversation.CandidateLocation) *models.BubbleContainer {
	buttons := make([]any, 0, len(candidates))
	for i, c := range candidates {
		style := "secondary"
		if i == 0 {
			style = "primary"
		}
		buttons = append(buttons, models.ButtonComponent{
			Type:   "button",
			Style:  style,
			Height: "sm",
			Action: &models.MessageAction{
				Type:  "message",
				Label: buttonLabel(c.DisplayName),
				Text:  strconv.Itoa(i + 1),
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
					Text:   "找到多個符合的地點",
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
		slog.Error("weather lookup by location", "city", city, "district", district, "error", err)
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
	h.locations.Remember(ctx, userID, forecast.City, forecast.District)
	reply := weather.FormatSingleForecast(forecast)
	if tip := h.advisor.Tip(ctx, forecast); tip != "" {
		reply += "建議：" + tip + "\n"
	}
	if note != "" {
		reply += note
	}
	_ = h.line.ReplyText(ctx, replyToken, strings.TrimRight(reply, "\n"), weatherQuickReplies()...)
}

// weatherQuickReplies 天氣回覆下方的快速按鈕.
func weatherQuickReplies() []models.QuickReplyItem {
	return []models.QuickReplyItem{
		models.LocationItem("傳送我的位置"),
		models.MessageItem("開啟天氣特報", keywordWeatherWarningOn),
		models.MessageItem("使用說明", keywordHelp),
	}
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

	selected := findClarifyCandidate(text, candidates)
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
		slog.Error("weather lookup after clarify", "city", selected.City, "district", selected.District, "error", err)
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
// 支援：數字索引（"1"、"2"...）或包含 district 名稱的文字.
// [F26-04] District 為空的候選不參與文字比對；「候選區名包含使用者打的字」只在使用者打 ≥2 個字時成立.
func findClarifyCandidate(text string, candidates []conversation.CandidateLocation) *conversation.CandidateLocation {
	text = strings.TrimSpace(text)

	// 嘗試數字選擇
	if idx, err := strconv.Atoi(text); err == nil {
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
