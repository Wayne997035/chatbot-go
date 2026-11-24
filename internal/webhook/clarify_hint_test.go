package webhook

import (
	"chatbot-go/internal/conversation"
	"context"
	"strings"
	"testing"
	"time"
)

// clarifyHintSentence [F37-04] 決策文案整句；刻意不引用 clarifyNoSuchOptionMessage，常數被改時測試要跟著紅.
const clarifyHintSentence = "沒有這個選項，請點選上方卡片的地點"

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// newClarifyBudgetFixture 真 Router + 假 Provider 的 fixture（AI 呼叫會被計次），對話狀態用可撥時鐘的記憶體 manager，
// 用來證明提示路徑沒有 Set（Set 會重設 5 分鐘到期）也沒有 Delete.
func newClarifyBudgetFixture(t *testing.T, cands []conversation.CandidateLocation) (*editFixture, *budgetProvider, *fakeClock) {
	t.Helper()
	p := newBudgetProvider()
	p.qa = []string{budgetQAAnswer}
	f := newBudgetFixture(t, p)
	clock := &fakeClock{now: time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)}
	f.h.convManager = conversation.NewMemoryManagerWithClock(clock.Now)
	setClarifyState(t, f.h, cands)
	return f, p, clock
}

// [F37-04] 選單開著時，只含數字且選不到候選的訊息：回固定提示、選單保留、不進 AI、不 Set 不 Delete（連送兩次也是）.
func TestLocationClarifyOutOfRangeNumberKeepsMenu(t *testing.T) {
	tests := []struct {
		name, text string
		cands      []conversation.CandidateLocation
	}{
		{"9 with 2 candidates", "9", xinyiBeitou},
		{"zero", "0", xinyiBeitou},
		{"overflowing digits", "99999999999999999999", xinyiBeitou},
		{"full-width 9", "９", xinyiBeitou},
		{"single candidate, 2", "2", xinyiBeitou[:1]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, p, clock := newClarifyBudgetFixture(t, tc.cands)

			clock.now = clock.now.Add(4 * time.Minute) // 距離到期只剩 1 分鐘
			for i := 0; i < 2; i++ {
				f.sayAs(t, "U1", tc.text)
				if texts := lastReplyTexts(t, f); len(texts) != 1 || texts[0] != clarifyHintSentence {
					t.Fatalf("send #%d: reply texts = %q, want exactly the hint", i+1, texts)
				}
				if strings.Contains(clarifyHintSentence, "1~") || strings.Contains(clarifyHintSentence, "請點 1") {
					t.Fatalf("hint must not mention numbers: %q", clarifyHintSentence)
				}
			}
			st, err := f.h.convManager.Get(context.Background(), "U1")
			if err != nil || st == nil || st.Type != conversation.TypeLocationClarify || len(st.Candidates) != len(tc.cands) {
				t.Fatalf("menu must stay: state=%+v err=%v", st, err)
			}
			if p.totalCalls() != 0 {
				t.Errorf("AI calls = %d, want 0", p.totalCalls())
			}
			// 沒有 Set：到期時間仍是原本那個，再過 2 分鐘（共 6 分鐘 > 5 分鐘 TTL）選單就該消失.
			clock.now = clock.now.Add(2 * time.Minute)
			if st, _ := f.h.convManager.Get(context.Background(), "U1"); st != nil {
				t.Errorf("TTL was refreshed by the hint path (a Set happened): %+v", st)
			}
		})
	}
}

// [F37-04] 選得到的數字照舊選中並清掉狀態（全形「２」只在數字分支轉半形）；「+2」「-1」維持 4281b68 的行為.
func TestLocationClarifyNumberSelectionUnchanged(t *testing.T) {
	selects := func(t *testing.T, text string) {
		t.Helper()
		f := newClarifyFixture(t, xinyiBeitouEntries)
		setClarifyState(t, f.h, xinyiBeitou)
		reply := f.say(t, text)
		if !strings.Contains(reply, "北投區") || strings.Contains(reply, "信義區") || strings.Contains(reply, clarifyHintSentence) {
			t.Errorf("%q should select 北投區, got: %s", text, reply)
		}
		assertNoState(t, f.h)
	}
	t.Run("half-width 2", func(t *testing.T) { selects(t, "2") })
	t.Run("full-width 2", func(t *testing.T) { selects(t, "２") })
	t.Run("plus 2 keeps selecting (Atoi quirk unchanged)", func(t *testing.T) { selects(t, "+2") })

	t.Run("minus 1 clears the menu and moves on without a hint", func(t *testing.T) {
		f := newClarifyFixture(t, xinyiBeitouEntries)
		setClarifyState(t, f.h, xinyiBeitou)
		reply := f.say(t, "-1")
		if strings.Contains(reply, clarifyHintSentence) || strings.Contains(reply, "北投區") {
			t.Errorf("-1 must not hint or select: %s", reply)
		}
		assertNoState(t, f.h)
	})
}

// [F37-04] 非數字文字照舊：選單作廢、沒有提示.
func TestLocationClarifyNonDigitTextStillClearsMenu(t *testing.T) {
	f := newClarifyFixture(t, xinyiBeitouEntries)
	setClarifyState(t, f.h, xinyiBeitou)
	reply := f.say(t, "別的話題")
	if strings.Contains(reply, clarifyHintSentence) {
		t.Errorf("non-digit text got the hint: %s", reply)
	}
	assertNoState(t, f.h)
}
