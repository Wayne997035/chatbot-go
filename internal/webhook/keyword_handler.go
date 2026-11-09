package webhook

import (
	"chatbot-go/internal/models"
)

// finishKeywords 結束對話的關鍵字.
var finishKeywords = []string{"完成", "好了", "不用了", "結束"}

// 關鍵字.
const (
	keywordHelp              = "說明"
	keywordWeatherWarningOn  = "開啟天氣特報"
	keywordQuakeOn           = "開啟地震海嘯警告"
	keywordQuakeOff          = "關閉地震海嘯警告"
	keywordWeatherWarningOff = "關閉天氣特報"
	keywordWeather           = "查天氣"
)

// helpAliases 都會顯示使用說明.
var helpAliases = map[string]bool{keywordHelp: true, "使用說明": true, "功能": true, "選單": true, "help": true, "?": true, "？": true}

// helpQuickReplies 說明與歡迎訊息下方的按鈕.
func helpQuickReplies() []models.QuickReplyItem {
	return []models.QuickReplyItem{
		models.LocationItem("傳送位置查天氣"),
		models.CameraItem("拍食物"),
		models.MessageItem("記運動", keywordLogWorkout),
		models.MessageItem("今日統計", keywordToday),
		models.MessageItem("開啟天氣特報", keywordWeatherWarningOn),
		models.MessageItem("開啟地震警告", keywordQuakeOn),
	}
}

// helpMessage 使用說明.
const helpMessage = "我可以幫你：\n" +
	"🌤 查天氣：輸入地名（如：大安區、天母、日月潭），或傳送位置\n" +
	"🍱 記飲食：拍照或直接打字（如：午餐吃了牛肉麵），估算熱量與營養素；「今日飲食」看今天吃了什麼\n" +
	"🏃 記運動：輸入「跑步 5公里 30分鐘」或傳運動 App 截圖，自動計算消耗熱量\n" +
	"📊 統計：「今日統計」「本週統計」看熱量收支；「體重 70 身高 170 30歲 男」設定身體資料\n" +
	"💬 問 AI：飲食、運動、天氣都可以直接打字問我\n" +
	"⚠️ 天氣特報：輸入「開啟天氣特報」選擇縣市，有特報時通知你\n" +
	"🌏 地震海嘯：輸入「開啟地震海嘯警告」，氣象署發布全台顯著有感地震報告或海嘯資訊時通知你\n" +
	"關閉通知：「關閉天氣特報」、「關閉地震海嘯警告」\n" +
	"刪除所有個人資料：「刪除我的資料」"
