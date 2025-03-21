package models

type LineResponse struct {
	ReplyToken string    `json:"replyToken"`
	Message    []Message `json:"messages"`
}
