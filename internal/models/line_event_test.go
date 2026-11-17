package models

import (
	"encoding/json"
	"strings"
	"testing"
)

// LINE 送來的位置與檔案訊息用數字表示座標與大小；欄位型別錯了整包事件都會解析失敗.
func TestWebhookPayloadWithLocationAndFileDecodes(t *testing.T) {
	body := `{"destination":"Uxxx","events":[
	 {"type":"message","replyToken":"r1","source":{"type":"user","userId":"U1"},
	  "message":{"id":"1","type":"location","title":"鼎泰豐 信義店","address":"台北市信義區松高路19號","latitude":25.0339,"longitude":121.5654}},
	 {"type":"message","replyToken":"r2","source":{"type":"user","userId":"U1"},
	  "message":{"id":"2","type":"file","fileName":"menu.pdf","fileSize":2138}}]}`
	var w EventWrapper
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(w.Events) != 2 {
		t.Fatalf("events = %d", len(w.Events))
	}
	loc := w.Events[0].Message
	if loc.Title != "鼎泰豐 信義店" || loc.Address != "台北市信義區松高路19號" || loc.Latitude != 25.0339 || loc.Longitude != 121.5654 {
		t.Errorf("location = %+v", loc)
	}
	if w.Events[1].Message.FileSize != 2138 {
		t.Errorf("file size = %d", w.Events[1].Message.FileSize)
	}
}

// [F33-19] LINE Messaging API reference「Postback event」段的官方範例原文
// （https://developers.line.biz/en/reference/messaging-api/#postback-event）；欄位一字不改，只把 data 換成本系統的按鈕資料.
const officialPostbackSample = `{
    "destination": "xxxxxxxxxx",
    "events": [
        {
            "replyToken": "b60d432864f44d079f6d8efe86cf404b",
            "type": "postback",
            "mode": "active",
            "source": {
                "userId": "U91eeaf62d...",
                "type": "user"
            },
            "timestamp": 1513669370317,
            "webhookEventId": "01FZ74A0TDDPYRVKNK77XKC3ZR",
            "deliveryContext": {
                "isRedelivery": false
            },
            "postback": {
                "data": "ce1:ok:AAAAAAAAAAAAAAAAAAAAAA",
                "params": {
                    "datetime": "2017-12-25T01:00"
                }
            }
        }
    ]
}`

// [F33-19] 官方範例要能解析：type、source.userId、postback.data 都讀得到.
func TestPostbackEventDecodesOfficialSample(t *testing.T) {
	var w EventWrapper
	if err := json.Unmarshal([]byte(officialPostbackSample), &w); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(w.Events) != 1 {
		t.Fatalf("events = %d", len(w.Events))
	}
	e := w.Events[0]
	if e.Type != "postback" || e.Source.UserID != "U91eeaf62d..." || e.Source.Type != "user" ||
		e.Postback.Data != "ce1:ok:AAAAAAAAAAAAAAAAAAAAAA" || e.ReplyToken != "b60d432864f44d079f6d8efe86cf404b" {
		t.Errorf("event = %+v", e)
	}
}

// [F33-19] postback 按鈕的 JSON：type=postback、帶 data 與 displayText，不帶 text（LINE 不允許並用）；displayText 可為空.
func TestPostbackItemJSON(t *testing.T) {
	tests := []struct {
		name        string
		item        QuickReplyItem
		wantHas     []string
		wantMissing []string
	}{
		{
			"with display text", PostbackItem("確認", "ce1:ok:x", "確認"),
			[]string{`"type":"postback"`, `"label":"確認"`, `"data":"ce1:ok:x"`, `"displayText":"確認"`},
			[]string{`"text"`},
		},
		{
			"empty display text is omitted", PostbackItem("取消", "ce1:no:x", ""),
			[]string{`"type":"postback"`, `"data":"ce1:no:x"`},
			[]string{`"text"`, `"displayText"`},
		},
		{
			"message item unchanged", MessageItem("今日統計", "今日統計"),
			[]string{`"type":"message"`, `"text":"今日統計"`},
			[]string{`"data"`, `"displayText"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.item)
			if err != nil {
				t.Fatal(err)
			}
			got := string(b)
			for _, s := range tt.wantHas {
				if !strings.Contains(got, s) {
					t.Errorf("%s missing %s", got, s)
				}
			}
			for _, s := range tt.wantMissing {
				if strings.Contains(got, s) {
					t.Errorf("%s must not contain %s", got, s)
				}
			}
		})
	}
}
