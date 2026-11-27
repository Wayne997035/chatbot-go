package models

import "log/slog"

// LINE Messaging API 事件結構.

type EventWrapper struct {
	Destination string  `json:"destination"`
	Events      []Event `json:"events"`
}

type Event struct {
	Type       string   `json:"type"`
	ReplyToken string   `json:"replyToken"`
	Timestamp  int64    `json:"timestamp"`
	Mode       string   `json:"mode"`
	Source     Source   `json:"source"`
	Message    Message  `json:"message"`
	Postback   Postback `json:"postback"`
}

type Source struct {
	Type    string `json:"type"`
	UserID  string `json:"userId"`
	GroupID string `json:"groupId,omitempty"`
	RoomID  string `json:"roomId,omitempty"`
}

type Message struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Address string `json:"address,omitempty"`
	Title   string `json:"title,omitempty"`
	// Latitude / Longitude / FileSize 在 LINE webhook 裡是數字；宣告成字串會讓整包事件解析失敗.
	Latitude  float64 `json:"latitude,omitempty"`
	Longitude float64 `json:"longitude,omitempty"`
	FileName  string  `json:"filename,omitempty"`
	FileSize  int64   `json:"fileSize,omitempty"`
	PackageID string  `json:"packageId,omitempty"`
	StickerID string  `json:"stickerId,omitempty"`
	// ContentProvider 圖片來源；只有 type 為 "line" 的內容能從 LINE 下載.
	ContentProvider ContentProvider `json:"contentProvider"`
}

// ContentProvider 圖片 / 影片的來源.
type ContentProvider struct {
	Type string `json:"type"`
}

type Postback struct {
	Data   string         `json:"data,omitempty"`
	Params PostbackParams `json:"params,omitempty"`
}

type PostbackParams struct {
	DateTime string `json:"datetime,omitempty"`
}

// LINE 訊息物件（Reply / Push 共用）.

// TextMessage 文字訊息，可附 Quick Reply 按鈕.
type TextMessage struct {
	Type       string      `json:"type"`
	Text       string      `json:"text"`
	QuickReply *QuickReply `json:"quickReply,omitempty"`
}

// MaxTextLen [F44-03] LINE 單則文字訊息的字數上限(以 LINE 的計法,見 TextLen).
const MaxTextLen = 5000

func runeLen(r rune) int {
	if r > 0xFFFF {
		return 2
	}
	return 1
}

// TextLen [F44-01] 依 LINE 的計法算字數:UTF-16 code unit,emoji 這類 BMP 以外的字算 2
// (https://developers.line.biz/en/docs/messaging-api/text-character-count/).
func TextLen(s string) int {
	n := 0
	for _, r := range s {
		n += runeLen(r)
	}
	return n
}

// TruncateText [F44-02] 截到 limit 個 LINE 字以內,超過時結尾補「…」;只在 rune 邊界切,不會把 emoji 切成一半.
// limit < 1 回空字串;沒超過原樣回傳.
func TruncateText(s string, limit int) string {
	if limit < 1 {
		return ""
	}
	if TextLen(s) <= limit {
		return s
	}
	used := 0
	for i, r := range s {
		w := runeLen(r)
		if used+w > limit-1 {
			return s[:i] + "…"
		}
		used += w
	}
	return s
}

// NewText 建立文字訊息；items 非空時附上 Quick Reply 按鈕.
func NewText(text string, items ...QuickReplyItem) TextMessage {
	if n := TextLen(text); n > MaxTextLen {
		slog.Warn("LINE text truncated", "len", n, "max", MaxTextLen) // [F44-08] 只記長度,內容是使用者資料或推播文字
		text = TruncateText(text, MaxTextLen)
	}
	msg := TextMessage{Type: "text", Text: text}
	if len(items) > 0 {
		msg.QuickReply = &QuickReply{Items: items}
	}
	return msg
}

// FlexMessage Flex 訊息.
type FlexMessage struct {
	Type       string      `json:"type"`
	AltText    string      `json:"altText"`
	Contents   any         `json:"contents"`
	QuickReply *QuickReply `json:"quickReply,omitempty"`
}

// QuickReply 訊息下方的快速回覆按鈕（最多 13 個）.
type QuickReply struct {
	Items []QuickReplyItem `json:"items"`
}

// QuickReplyItem 一個快速回覆按鈕.
type QuickReplyItem struct {
	Type   string `json:"type"`
	Action Action `json:"action"`
}

// Action LINE 動作（message / location / camera / cameraRoll / postback）.
type Action struct {
	Type  string `json:"type"`
	Label string `json:"label"`
	Text  string `json:"text,omitempty"`
	URI   string `json:"uri,omitempty"`
	// Data / DisplayText [F33-19] postback 動作用；LINE 規定 displayText 與 text 不可並用，所以 postback 按鈕不帶 Text.
	Data        string `json:"data,omitempty"`
	DisplayText string `json:"displayText,omitempty"`
}

// URIItem 開啟網址（例如 LIFF 統計頁）的按鈕.
func URIItem(label, uri string) QuickReplyItem {
	return QuickReplyItem{Type: "action", Action: Action{Type: "uri", Label: label, URI: uri}}
}

// MessageItem 點了會替使用者送出 text 的按鈕.
func MessageItem(label, text string) QuickReplyItem {
	return QuickReplyItem{Type: "action", Action: Action{Type: "message", Label: label, Text: text}}
}

// PostbackItem [F33-19] 點了會把 data 以 postback 事件回傳給 webhook 的按鈕（手打文字無法偽造）；
// displayText 空字串 = 聊天室不顯示替使用者送出的文字.
func PostbackItem(label, data, displayText string) QuickReplyItem {
	return QuickReplyItem{Type: "action", Action: Action{Type: "postback", Label: label, Data: data, DisplayText: displayText}}
}

// LocationItem 開啟位置分享的按鈕.
func LocationItem(label string) QuickReplyItem {
	return QuickReplyItem{Type: "action", Action: Action{Type: "location", Label: label}}
}

// CameraItem 開啟相機的按鈕.
func CameraItem(label string) QuickReplyItem {
	return QuickReplyItem{Type: "action", Action: Action{Type: "camera", Label: label}}
}

// CameraRollItem 開啟相簿的按鈕.
func CameraRollItem(label string) QuickReplyItem {
	return QuickReplyItem{Type: "action", Action: Action{Type: "cameraRoll", Label: label}}
}

type BubbleContainer struct {
	Type   string        `json:"type"`
	Body   *BoxComponent `json:"body,omitempty"`
	Footer *BoxComponent `json:"footer,omitempty"`
}

type BoxComponent struct {
	Type     string `json:"type"`
	Layout   string `json:"layout"`
	Spacing  string `json:"spacing,omitempty"`
	Contents []any  `json:"contents"`
}

type TextComponent struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Weight string `json:"weight,omitempty"`
	Size   string `json:"size,omitempty"`
	Color  string `json:"color,omitempty"`
}

type ButtonComponent struct {
	Type   string         `json:"type"`
	Style  string         `json:"style,omitempty"`
	Height string         `json:"height,omitempty"`
	Action *MessageAction `json:"action"`
}

type MessageAction struct {
	Type  string `json:"type"`
	Label string `json:"label"`
	Text  string `json:"text"`
}
