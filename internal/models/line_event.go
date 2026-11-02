package models

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
	ID        string `json:"id"`
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Address   string `json:"address,omitempty"`
	Title     string `json:"title,omitempty"`
	Latitude  string `json:"latitude,omitempty"`
	Longitude string `json:"longitude,omitempty"`
	FileName  string `json:"filename,omitempty"`
	FileSize  string `json:"fileSize,omitempty"`
	PackageID string `json:"packageId,omitempty"`
	StickerID string `json:"stickerId,omitempty"`
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

// NewText 建立文字訊息；items 非空時附上 Quick Reply 按鈕.
func NewText(text string, items ...QuickReplyItem) TextMessage {
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
}

// MessageItem 點了會替使用者送出 text 的按鈕.
func MessageItem(label, text string) QuickReplyItem {
	return QuickReplyItem{Type: "action", Action: Action{Type: "message", Label: label, Text: text}}
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
