package models

import (
	"encoding/json"
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
