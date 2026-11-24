package webhook

import (
	"chatbot-go/internal/models"
	"regexp"
	"strings"
	"testing"
)

var reqHex16 = regexp.MustCompile(`^[0-9a-f]{16}$`)

func reqidTextEvent(userID, text string) *models.Event {
	return &models.Event{
		Type: "message", ReplyToken: "tok", Mode: "active",
		Source:  models.Source{Type: "user", UserID: userID},
		Message: models.Message{ID: "m1", Type: "text", Text: text},
	}
}

// [F37-08] 經 handleEventSafely 送一則文字事件：恰好一筆 INFO「text message handled」，step 是實際處理的步驟，req 是 16 hex；
// 且新 log 行不含 userId 與訊息原文. 直呼 handleTextMessage 拿不到 req（req 只在 handleEventSafely 掛上），所以一律走事件入口.
func TestTextEventLogsExactlyOneStepLine(t *testing.T) {
	const uid = "Uf37privacy0123456789abcdef012345"
	tests := []struct {
		name     string
		text     string
		wantStep string
	}{
		{"workout step", "打了一場球", "workout"},
		{"empty text", "   \n ", "emptyText"},
		{"too long", strings.Repeat("字", maxInputRunes+1), "tooLong"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureSlog(t)
			f := newBudgetFixture(t, newBudgetProvider())
			f.h.userRepo = newFakeUserRepo() // saveUser 需要 user repo

			f.h.handleEventSafely(t.Context(), reqidTextEvent(uid, tc.text))

			recs := recordsByMsg(t, buf, "text message handled")
			if len(recs) != 1 {
				t.Fatalf("got %d 'text message handled' lines, want exactly 1: %s", len(recs), buf.String())
			}
			r := recs[0]
			if r["level"] != "INFO" {
				t.Errorf("level = %v, want INFO", r["level"])
			}
			if r["step"] != tc.wantStep {
				t.Errorf("step = %v, want %q", r["step"], tc.wantStep)
			}
			if req, _ := r["req"].(string); !reqHex16.MatchString(req) {
				t.Errorf("req = %v, want 16 lower-case hex", r["req"])
			}
			for _, line := range strings.Split(buf.String(), "\n") {
				if !strings.Contains(line, `"text message handled"`) {
					continue
				}
				for _, secret := range []string{uid, strings.TrimSpace(tc.text)} {
					if secret != "" && strings.Contains(line, secret) {
						t.Errorf("step line leaks %q: %s", secret, line)
					}
				}
			}
		})
	}
}

// [F37-08] 同一個 handler 連送兩則：兩個 req 不同.
func TestTwoEventsGetDistinctReqIDs(t *testing.T) {
	buf := captureSlog(t)
	f := newBudgetFixture(t, newBudgetProvider())
	f.h.userRepo = newFakeUserRepo() // saveUser 需要 user repo

	f.h.handleEventSafely(t.Context(), reqidTextEvent("U1", "打了一場球"))
	f.h.handleEventSafely(t.Context(), reqidTextEvent("U1", "打了一場球"))

	recs := recordsByMsg(t, buf, "text message handled")
	if len(recs) != 2 {
		t.Fatalf("got %d step lines, want 2", len(recs))
	}
	a, _ := recs[0]["req"].(string)
	b, _ := recs[1]["req"].(string)
	if !reqHex16.MatchString(a) || !reqHex16.MatchString(b) || a == b {
		t.Errorf("req ids = %q, %q; want two distinct 16-hex ids", a, b)
	}
}
