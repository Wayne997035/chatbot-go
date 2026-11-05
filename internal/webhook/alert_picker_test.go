package webhook

import (
	"chatbot-go/internal/models"
	"chatbot-go/internal/region"
	"encoding/json"
	"testing"
	"unicode/utf8"
)

// maxBubbleBytes LINE 一張 bubble 的 JSON 上限（Messaging API reference：「The maximum size of JSON
// data that defines a bubble is 30 KB.」）.
const maxBubbleBytes = 30 * 1024

// bubbleActions 依序取出卡片裡每顆按鈕的動作.
func bubbleActions(t *testing.T, b *models.BubbleContainer) []models.MessageAction {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= maxBubbleBytes {
		t.Errorf("bubble is %d bytes, LINE limit is %d", len(raw), maxBubbleBytes)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	var out []models.MessageAction
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if x["type"] == "button" {
				a, _ := x["action"].(map[string]any)
				label, _ := a["label"].(string)
				text, _ := a["text"].(string)
				typ, _ := a["type"].(string)
				out = append(out, models.MessageAction{Type: typ, Label: label, Text: text})
				return
			}
			for _, k := range []string{"body", "footer", "contents"} {
				if c, ok := x[k]; ok {
					walk(c)
				}
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(tree)
	for _, a := range out {
		if a.Type != "message" || utf8.RuneCountInString(a.Label) > 40 {
			t.Errorf("button %+v: want a message action with a label of at most 40 characters", a)
		}
	}
	return out
}

// [F24-98] 縣市卡依序列 22 縣市，按鈕送「選擇+縣市」，另有「完成」送「完成訂閱」.
func TestCountyPickerListsAllCountiesAndFinish(t *testing.T) {
	actions := bubbleActions(t, countyPickerBubble())
	if len(actions) != len(region.Counties)+1 {
		t.Fatalf("got %d buttons, want %d counties + 完成", len(actions), len(region.Counties))
	}
	for i, c := range region.Counties {
		if actions[i].Label != c || actions[i].Text != "選擇"+c {
			t.Errorf("button %d = %+v, want label %q text %q", i, actions[i], c, "選擇"+c)
		}
	}
	if last := actions[len(actions)-1]; last.Label != "完成" || last.Text != "完成訂閱" {
		t.Errorf("finish button = %+v", last)
	}
}

// [F24-98] 每個縣市的鄉鎮卡：第一顆是整個縣市，其餘是該縣全部鄉鎮；最大的高雄市也在 30 KB 內.
func TestTownshipPickerForEveryCounty(t *testing.T) {
	for _, c := range region.Counties {
		actions := bubbleActions(t, townshipPickerBubble(c))
		towns := region.Townships(c)
		if len(actions) != 1+len(towns) {
			t.Errorf("%s: %d buttons, want 1 + %d", c, len(actions), len(towns))
			continue
		}
		if actions[0].Label != "整個"+c || actions[0].Text != "訂閱整個"+c {
			t.Errorf("%s: first button = %+v", c, actions[0])
		}
		for i, town := range towns {
			if a := actions[i+1]; a.Label != town || a.Text != "訂閱"+c+town {
				t.Errorf("%s: button %d = %+v, want label %q text %q", c, i+1, a, town, "訂閱"+c+town)
			}
		}
	}
}
