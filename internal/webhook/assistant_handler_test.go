package webhook

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/assistant"
	"chatbot-go/internal/conversation"
	"chatbot-go/internal/weather"
	"context"
	"errors"
	"strings"
	"testing"
)

type qaCompleter struct {
	answer string
	asked  []string
}

func (q *qaCompleter) Complete(_ context.Context, req *ai.Request) ai.Result {
	q.asked = append(q.asked, req.Prompt)
	if q.answer == "" || (req.Accept != nil && !req.Accept(q.answer)) {
		return ai.Result{Errors: []string{"claude(claude-opus-5-5) 529: overloaded org_secret999999"}}
	}
	return ai.Result{Text: q.answer, Provider: "claude"}
}

type qaFixture struct {
	h   *WebhookHandler
	api *lineAPI
	ai  *qaCompleter
}

func newQAFixture(t *testing.T, answer string) *qaFixture {
	t.Helper()
	f := &qaFixture{api: newLineAPI(t, nil), ai: &qaCompleter{answer: answer}}
	f.h = NewWebhookHandler(nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil, conversation.NewMemoryManager()).
		WithAssistant(assistant.New(&assistant.Config{Completer: f.ai}), nil)
	return f
}

func (f *qaFixture) say(t *testing.T, text string) string {
	t.Helper()
	f.h.handleTextMessage(context.Background(), "U1", text, "tok")
	return f.api.lastReply(t)
}

func TestUnmatchedTextIsAnsweredByAI(t *testing.T) {
	f := newQAFixture(t, "晚餐可以選雞胸肉沙拉，約 450 大卡。")
	reply := f.say(t, "晚餐吃什麼比較好")
	if !strings.Contains(reply, "雞胸肉沙拉") || strings.Contains(reply, "找不到") {
		t.Fatalf("reply: %s", reply)
	}
	if f.api.loading != 1 || len(f.ai.asked) != 1 || !strings.Contains(f.ai.asked[0], "【問題】\n晚餐吃什麼比較好") {
		t.Errorf("loading=%d asked=%v", f.api.loading, f.ai.asked)
	}
}

func TestAskButtonAndPrefix(t *testing.T) {
	f := newQAFixture(t, "可以，記得暖身。")
	if reply := f.say(t, "問 AI"); !strings.Contains(reply, "想問什麼") || !strings.Contains(reply, "今天適合跑步嗎") || len(f.ai.asked) != 0 {
		t.Fatalf("ask button: %s", reply)
	}
	// 「問」前綴即使內容像關鍵字也直接當問題.
	if reply := f.say(t, "問 今日統計是怎麼算的"); !strings.Contains(reply, "記得暖身") {
		t.Fatalf("prefix: %s", reply)
	}
	if !strings.Contains(f.ai.asked[0], "【問題】\n今日統計是怎麼算的") {
		t.Errorf("question = %s", f.ai.asked[0])
	}
}

func TestAIFailureShowsPlainMessage(t *testing.T) {
	f := newQAFixture(t, "")
	reply := f.say(t, "今天適合跑步嗎")
	if !strings.Contains(reply, "目前無法回答") {
		t.Fatalf("reply: %s", reply)
	}
	for _, leak := range []string{"529", "org_", "claude", "overloaded"} {
		if strings.Contains(reply, leak) {
			t.Errorf("AI error leaked to the user: %s", reply)
		}
	}
}

func TestWithoutAssistantKeepsNotFoundReply(t *testing.T) {
	api := newLineAPI(t, nil)
	h := NewWebhookHandler(nil, weather.NewLookup(&emptyWeatherRepo{}, nil, 60), nil, conversation.NewMemoryManager())
	h.handleTextMessage(context.Background(), "U1", "火星", "tok")
	if reply := api.lastReply(t); !strings.Contains(reply, "找不到「火星」") {
		t.Errorf("reply: %s", reply)
	}
	h.handleTextMessage(context.Background(), "U1", "問 AI", "tok")
	if reply := api.lastReply(t); !strings.Contains(reply, "未啟用") {
		t.Errorf("ask button without AI: %s", reply)
	}
}

func TestDeleteMyDataAlsoClearsQAHistory(t *testing.T) {
	f := newFitnessFixture(t, "")
	qa := &qaCompleter{answer: "好的"}
	hist := &recordingHistory{}
	f.h.WithAssistant(assistant.New(&assistant.Config{Completer: qa, History: hist}), nil)
	f.say(t, keywordDeleteData)
	f.say(t, "確認刪除")
	if !hist.cleared {
		t.Error("Q&A history must be cleared with the rest of the personal data")
	}
	if reply := f.api.lastReply(t); !strings.Contains(reply, "已刪除") {
		t.Errorf("reply: %s", reply)
	}
}

// 對話紀錄刪不掉時不能回「已刪除」，要請使用者重試.
func TestDeleteMyDataReportsHistoryFailure(t *testing.T) {
	f := newFitnessFixture(t, "")
	hist := &recordingHistory{err: errors.New("redis down")}
	f.h.WithAssistant(assistant.New(&assistant.Config{Completer: &qaCompleter{answer: "好的"}, History: hist}), nil)
	f.say(t, keywordDeleteData)
	f.say(t, "確認刪除")
	if reply := f.api.lastReply(t); !strings.Contains(reply, "部分資料刪除失敗") {
		t.Errorf("reply: %s", reply)
	}
}

type recordingHistory struct {
	cleared bool
	err     error
}

func (r *recordingHistory) Recent(context.Context, string) []assistant.Turn { return nil }
func (r *recordingHistory) Append(context.Context, string, assistant.Turn)  {}

func (r *recordingHistory) Clear(context.Context, string) error {
	r.cleared = true
	return r.err
}
