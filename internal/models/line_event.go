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
}

type Postback struct {
	Data   string         `json:"data,omitempty"`
	Params PostbackParams `json:"params,omitempty"`
}

type PostbackParams struct {
	DateTime string `json:"datetime,omitempty"`
}

// LINE Reply API 結構.

type ReplyMessage struct {
	ReplyToken string      `json:"replyToken"`
	Messages   []ReplyBody `json:"messages"`
}

type ReplyBody struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
