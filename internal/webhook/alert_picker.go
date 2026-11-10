package webhook

import (
	"chatbot-go/internal/models"
	"chatbot-go/internal/region"
	"strings"
)

// 天氣特報選地區的卡片.
// [F24-98] 按鈕送出只有卡片會送的文字：對話狀態過期後點到舊卡片，才分得出是點卡片、不是要查天氣.
const (
	pickCountyPrefix  = "選擇"   // 縣市卡：選擇台北市
	pickAreaPrefix    = "訂閱"   // 鄉鎮卡：訂閱台北市中正區、訂閱整個台北市
	wholeCountyPrefix = "整個"   // 鄉鎮卡第一顆
	keywordPickFinish = "完成訂閱" // 縣市卡與快速回覆的「完成」
	pickButtonsPerRow = 3
)

// pickKind 選地區時這則文字是哪一種.
type pickKind int

const (
	pickUnknown     pickKind = iota
	pickCounty               // 選了縣市 → 回該縣鄉鎮卡
	pickWholeCounty          // 整個縣市
	pickTownship             // 縣市+鄉鎮
)

// parsePick 解析選地區的文字；卡片前綴可有可無（手打「台北市」「台北市大安區」也行）.
func parsePick(text string) (kind pickKind, county, town string) {
	if c := strings.TrimPrefix(text, pickCountyPrefix); region.IsCounty(c) {
		return pickCounty, c, ""
	}
	rest := strings.TrimPrefix(text, pickAreaPrefix)
	if c, ok := strings.CutPrefix(rest, wholeCountyPrefix); ok && region.IsCounty(c) {
		return pickWholeCounty, c, ""
	}
	if c, t, ok := region.SplitArea(rest); ok {
		return pickTownship, c, t
	}
	return pickUnknown, "", ""
}

// isPickerTap 只有卡片才會送出的文字（不含手打的縣市名）；對話過期後用來提示重新開始.
func isPickerTap(text string) bool {
	if text == keywordPickFinish {
		return true
	}
	if c, ok := strings.CutPrefix(text, pickCountyPrefix); ok && region.IsCounty(c) {
		return true
	}
	rest, ok := strings.CutPrefix(text, pickAreaPrefix)
	if !ok {
		return false
	}
	if c, whole := strings.CutPrefix(rest, wholeCountyPrefix); whole && region.IsCounty(c) {
		return true
	}
	_, _, isArea := region.SplitArea(rest)
	return isArea
}

// finishQuickReply 選地區時每則回覆下方的「完成」.
func finishQuickReply() models.QuickReplyItem {
	return models.MessageItem("完成", keywordPickFinish)
}

// countyPickerBubble 22 縣市按鈕 + 「完成」.
func countyPickerBubble() *models.BubbleContainer {
	buttons := make([]models.ButtonComponent, 0, len(region.Counties))
	for _, c := range region.Counties {
		buttons = append(buttons, pickButton(c, pickCountyPrefix+c, "secondary"))
	}
	return pickerBubble("選擇要訂閱天氣特報的縣市",
		"可以選多個，選了縣市還能再選鄉鎮；也可以直接打字。選好按「完成」",
		buttons, &models.BoxComponent{
			Type: "box", Layout: "vertical",
			Contents: []any{pickButton("完成", keywordPickFinish, "primary")},
		})
}

// townshipPickerBubble 「整個縣市」+ 該縣每個鄉鎮.
func townshipPickerBubble(county string) *models.BubbleContainer {
	towns := region.Townships(county)
	buttons := make([]models.ButtonComponent, 0, 1+len(towns))
	buttons = append(buttons, pickButton(wholeCountyPrefix+county, pickAreaPrefix+wholeCountyPrefix+county, "primary"))
	for _, t := range towns {
		buttons = append(buttons, pickButton(t, pickAreaPrefix+county+t, "secondary"))
	}
	return pickerBubble(county+"：選擇鄉鎮市區",
		"可以選整個縣市，也可以只選幾個鄉鎮；豪大雨、低溫、強風特報會照鄉鎮通知",
		buttons, nil)
}

func pickButton(label, text, style string) models.ButtonComponent {
	return models.ButtonComponent{
		Type: "button", Style: style, Height: "sm",
		Action: &models.MessageAction{Type: actionTypeMessage, Label: label, Text: text},
	}
}

// pickerBubble 標題、說明，按鈕每列三顆（最後一列用空 box 補齊寬度）.
func pickerBubble(title, hint string, buttons []models.ButtonComponent, footer *models.BoxComponent) *models.BubbleContainer {
	contents := []any{
		models.TextComponent{Type: "text", Text: title, Weight: "bold", Size: "md"},
		models.TextComponent{Type: "text", Text: hint, Size: "xs", Color: "#888888"},
	}
	for i := 0; i < len(buttons); i += pickButtonsPerRow {
		row := []any{}
		for j := i; j < i+pickButtonsPerRow; j++ {
			if j < len(buttons) {
				row = append(row, buttons[j])
			} else {
				row = append(row, models.BoxComponent{Type: "box", Layout: "vertical", Contents: []any{}})
			}
		}
		contents = append(contents, models.BoxComponent{Type: "box", Layout: "horizontal", Spacing: "sm", Contents: row})
	}
	return &models.BubbleContainer{
		Type:   "bubble",
		Body:   &models.BoxComponent{Type: "box", Layout: "vertical", Spacing: "sm", Contents: contents},
		Footer: footer,
	}
}
