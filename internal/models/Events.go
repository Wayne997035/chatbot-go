package models

type Events struct {
	Type            string          `json:"type"`
	Message         Message         `json:"message,omitempty"`
	Timestamp       int64           `json:"timestamp"`
	Source          Source          `json:"source"`
	ReplyToken      string          `json:"replyToken,omitempty"`
	Mode            string          `json:"mode"`
	WebhookEventID  string          `json:"webhookEventId"`
	DeliveryContext DeliveryContext `json:"deliveryContext"`
}
