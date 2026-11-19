package assistant

import (
	"chatbot-go/internal/assistant/recordedit"
	"context"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// [F34-02] 問答誠實過濾與固定句的測試.
// 預期值取自需求（問答誠實規格驗收 #1-#17 與開工單），不取自實作輸出.

// 規格 #1-#7 的正式環境原句（user 截圖，逐字，不得改寫或縮短）.
const (
	prodLie1 = "我無法直接修改或重新整理你的紀錄分組。請你直接告訴我具體要調整哪一筆，例如「把牛肉三明治改為早餐」，系統就會跳出確認按鈕供你按下更新。"
	prodLie2 = "沒問題，請直接輸入「把牛肉火腿起司三明治改為早餐」，系統就會跳出確認按鈕供你按下更新。"
	prodLie3 = "收到，系統已送出修改確認。"
	prodLie4 = "請留意聊天室彈出的確認按鈕，按下後這筆紀錄就會正式歸類為早餐。"
	prodLie5 = "這樣你的三餐分組就會更清晰了。"
	prodLie7 = "關於顯示上的分組問題，建議你下次記錄時，可以明確標註「早餐：…」、「午餐：…」，這樣系統更容易識別並分開統計"
	// 規格第 3、4、5 列三句依序直接串接（不加分隔）；reply 4 與它逐字相同.
	prodReply3 = prodLie3 + prodLie4 + prodLie5
)

// TestQAHonestyProductionSentences [F34-02] 規格 #1-#7：正式環境謊稱原句逐字當輸入.
func TestQAHonestyProductionSentences(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantRemoved int
		wantOut     string   // 非空 = 輸出要完全相同
		wantPrefix  string   // 非空 = 輸出要以此開頭（保留的真話）
		banned      []string // 輸出不得含
	}{
		{"#1 保留第一句真話、刪承諾句", prodLie1, 1, "", "我無法直接修改或重新整理你的紀錄分組。", []string{"就會跳出確認按鈕", "確認按鈕"}},
		{"#2 整句都是謊話 → 只剩 notice", prodLie2, 1, qaHonestyNotice, "", []string{"系統就會跳出確認按鈕", "改為早餐"}},
		{"#3 已送出修改確認", prodLie3, 1, qaHonestyNotice, "", []string{"系統已送出修改確認"}},
		{"#4 彈出的確認按鈕", prodLie4, 1, qaHonestyNotice, "", []string{"確認按鈕"}},
		{"#5 第三句沒有宣稱要留", prodLie5, 0, prodLie5, "", nil},
		{"#5 reply 3 全文", prodReply3, 2, prodLie5 + "\n" + qaHonestyNotice, "", []string{"已送出", "確認按鈕"}},
		{"#6 reply 4 與 reply 3 逐字相同", prodReply3, 2, prodLie5 + "\n" + qaHonestyNotice, "", []string{"已送出", "確認按鈕"}},
		{"#7 教「早餐：…」格式（沒有句號）", prodLie7, 1, qaHonestyNotice, "", []string{"早餐：…"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, removed := filterRecordClaims(tc.in)
			if removed != tc.wantRemoved {
				t.Fatalf("removed = %d, want %d; out=%q", removed, tc.wantRemoved, got)
			}
			if tc.wantOut != "" && got != tc.wantOut {
				t.Errorf("out = %q, want %q", got, tc.wantOut)
			}
			if tc.wantPrefix != "" && !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("out = %q, must keep the true first sentence %q", got, tc.wantPrefix)
			}
			for _, b := range tc.banned {
				if strings.Contains(got, b) {
					t.Errorf("out must not contain %q: %q", b, got)
				}
			}
			wantNotices := 1
			if tc.wantRemoved == 0 {
				wantNotices = 0
			}
			if n := strings.Count(got, qaHonestyNotice); n != wantNotices {
				t.Errorf("qaHonestyNotice count = %d, want %d: %q", n, wantNotices, got)
			}
		})
	}
}

// TestQAHonestyBeforeHistory [F34-02] 規格 #8：過濾早於寫 history，下一輪 prompt 不會把謊話當範例.
func TestQAHonestyBeforeHistory(t *testing.T) {
	lies := []string{prodLie1, prodLie2, prodLie3, prodLie4, prodLie7}
	bannedAll := []string{"已送出修改確認", "跳出確認按鈕", "彈出的確認按鈕", "標註「早餐"}
	c := &fakeCompleter{}
	a, h := newTestAssistant(c, false)
	for i, lie := range lies {
		c.answers = []string{lie}
		got, err := a.Answer(context.Background(), "U1", "把牛肉三明治改為早餐")
		if err != nil {
			t.Fatalf("answer %d: %v", i, err)
		}
		for _, b := range bannedAll {
			if strings.Contains(got.Text, b) {
				t.Errorf("reply %d contains %q: %q", i, b, got.Text)
			}
		}
		turns := h.turns["U1"]
		if last := turns[len(turns)-1]; last.Answer != got.Text {
			t.Errorf("history must hold the cleaned reply: history=%q reply=%q", last.Answer, got.Text)
		}
	}
	// 下一輪 prompt 的「先前對話」也不含謊話.
	c.answers = []string{"好的"}
	if _, err := a.Answer(context.Background(), "U1", "再說一次"); err != nil {
		t.Fatal(err)
	}
	prompt := c.reqs[len(c.reqs)-1].Prompt
	for _, b := range bannedAll {
		if strings.Contains(prompt, b) {
			t.Errorf("next-round prompt must not carry the lie %q:\n%s", b, prompt)
		}
	}
}

// TestQAHonestyVariants [F34-02] 規格 #9：同型謊稱的常見說法各一子測試；使用者自己做的事不算.
// 最後三句是「只有單一規則才接得住」的句子（Lead 以規格 T2 參考式實測，各只命中一條），是突變測試的指定點.
func TestQAHonestyVariants(t *testing.T) {
	lies := []string{
		"已發送確認，請稍候。", "已幫你送出修改申請。", "稍後會出現確認按鈕。",
		"確認卡片馬上出現。", "系統會跳出一張確認卡讓你選。", "已提交修改請求。",
		"這筆會用確認卡讓你選。",  // 只命中規則 1
		"按鈕等一下就會跳出來。",  // 只命中規則 2
		"系統會出現確認。",     // 只命中規則 6
		"點一下確認就好。",     // 只命中規則 4
		"稍後會出現確認，請按下。", // 只命中規則 5
	}
	for _, lie := range lies {
		t.Run(lie, func(t *testing.T) {
			if got, n := filterRecordClaims(lie); n != 1 {
				t.Errorf("removed = %d, want 1; out=%q", n, got)
			}
		})
	}
	for _, keep := range []string{"你已經送出確認了。", "你已送出的修改我看到了。"} {
		t.Run("負對照 "+keep, func(t *testing.T) {
			if got, n := filterRecordClaims(keep); n != 0 || got != keep {
				t.Errorf("user's own action must stay: removed=%d out=%q", n, got)
			}
		})
	}
}

// TestQAHonestyKeepsLegit [F34-02] 規格 #10-#13、#15：正當句子原樣保留.
func TestQAHonestyKeepsLegit(t *testing.T) {
	keeps := []string{
		// #10
		"我無法直接修改或重新整理你的紀錄分組。", "請確認你的體重是否輸入正確。", "確認一下，你說的是昨天的牛肉麵嗎？",
		"如果出現胸悶或頭暈，請確認自己的狀況並盡快就醫。",
		// #11 真實存在的「記成一餐」按鈕
		"你可以點下方「記成一餐」按鈕把它記下來。",
		// #12 天氣、運動句
		"稍後會出現陣雨，記得帶傘。", "按下開始鍵後跑步 30 分鐘。", "請按照食物份量估算熱量。",
		// #13 引用使用者資料、加上不接餐別冒號
		"今天早餐：燕麥 300 大卡，午餐：雞腿便當 800 大卡。", "建議早餐加上一顆蛋，蛋白質更夠。",
		// #15 規則 3 估算流程（PR#33）
		"我估算約 210 大卡，要不要幫你把這一筆改成這個數字？", "紀錄是 150 大卡，依你的描述估算約 210 大卡。",
		"要改的話回「改」。",
	}
	for _, keep := range keeps {
		t.Run(keep, func(t *testing.T) {
			if got, n := filterRecordClaims(keep); n != 0 || got != keep {
				t.Errorf("legit sentence changed: removed=%d out=%q", n, got)
			}
		})
	}
}

// TestQAHonestyIdempotent [F34-02] 規格 #14：程式自己產的句子不會被自己的過濾器刪；濾兩次等於濾一次.
func TestQAHonestyIdempotent(t *testing.T) {
	for name, own := range map[string]string{
		"editClaimNotice": editClaimNotice, "recordClaimNotice": recordClaimNotice, "qaHonestyNotice": qaHonestyNotice,
		"editableListReply": editableListReply, "editOfferConsumedNote": editOfferConsumedNote,
	} {
		if got, n := filterRecordClaims(own); n != 0 || got != own {
			t.Errorf("%s must survive the filter: removed=%d out=%q", name, n, got)
		}
	}
	for _, in := range []string{prodLie1, prodLie2, prodLie3, prodLie4, prodReply3, prodLie7} {
		once, _ := filterRecordClaims(in)
		twice, n := filterRecordClaims(once)
		if n != 0 || twice != once {
			t.Errorf("filter twice != once for %q: once=%q twice=%q removed=%d", in, once, twice, n)
		}
		if c := strings.Count(twice, qaHonestyNotice); c != 1 {
			t.Errorf("qaHonestyNotice count = %d, want 1: %q", c, twice)
		}
	}
}

// TestQAHonestyNoticeSurvivesTruncation [F34-02] 超長回答 + 謊稱句：截斷後結尾仍完整含 qaHonestyNotice 恰一次.
func TestQAHonestyNoticeSurvivesTruncation(t *testing.T) {
	long := strings.Repeat("牛肉麵大約 550 大卡。", 200) // 遠超 maxAnswerRunes
	if utf8.RuneCountInString(long) <= maxAnswerRunes {
		t.Fatal("fixture must exceed the answer limit")
	}
	for name, raw := range map[string]string{
		"只有謊稱": long + prodLie3,
		"同時有 editClaim 與謊稱（只附 notice）": long + "已將今天的熱量紀錄更新為 700 大卡。" + prodLie3,
	} {
		t.Run(name, func(t *testing.T) {
			got, _, removed := finalize(raw)
			if removed < 1 {
				t.Fatalf("removed = %d", removed)
			}
			if !strings.HasSuffix(got, qaHonestyNotice) || strings.Count(got, qaHonestyNotice) != 1 {
				t.Errorf("must end with exactly one qaHonestyNotice: %q", got[max(0, len(got)-120):])
			}
			if strings.Contains(got, editClaimNotice) {
				t.Errorf("only qaHonestyNotice may close the answer, not both notices")
			}
			if n := utf8.RuneCountInString(got); n > maxAnswerRunes {
				t.Errorf("answer = %d runes, limit %d", n, maxAnswerRunes)
			}
		})
	}
}

// TestSystemPromptNoFakeConfirmPromise [F34-01] 規格 #17：提示詞不再承諾確認按鈕，且明講做不到與沒改到.
func TestSystemPromptNoFakeConfirmPromise(t *testing.T) {
	for _, want := range []string{"不能送出任何確認", "這次我沒有改到任何紀錄", "不要建議客人改用"} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("system prompt must contain %q", want)
		}
	}
	for _, banned := range []string{"系統會出確認按鈕", "按下確認才會寫入", "系統就會跳出"} {
		if strings.Contains(systemPrompt, banned) {
			t.Errorf("system prompt must not contain the old lie %q", banned)
		}
	}
	// 清單與例句來自 editableFeatures，提示詞和固定句同一份.
	if !strings.Contains(systemPrompt, editableFeaturesText()) || !strings.Contains(systemPrompt, "「"+editableExample+"」") {
		t.Error("system prompt must embed the editableFeatures list and the example")
	}
}

// TestSystemPromptReestimateNotReplacedByList [F34-01] 修改單 fix/r3/user-liveeval-REPLAY-01：
// 客人補充新資訊或要重新估算時，估算不能被「沒改到＋清單」腳本取代（真模型重播：main 綠、PR34 紅的三支都是這條路）.
// 清單那段要排在重新估算那段後面，模型讀到清單前已經知道重新估算不走清單.
func TestSystemPromptReestimateNotReplacedByList(t *testing.T) {
	const reestimate = "估算數字一定要給、不能拿清單代替"
	at := strings.Index(systemPrompt, reestimate)
	if at < 0 {
		t.Fatalf("system prompt must contain %q", reestimate)
	}
	if list := strings.Index(systemPrompt, editableFeaturesText()); list < at {
		t.Errorf("re-estimate rule (at %d) must come before the editable list (at %d)", at, list)
	}
	// 第二次重播：「請調整」被當成其他修改 → 觸發條件要是具體的新資訊、不看客人有沒有叫你改，且「沒改到」排在估算之後.
	for _, want := range []string{"吃了兩個、只吃一半、有包餡", "不管有沒有叫你改紀錄", "最後才補一句「這次我沒有改到任何紀錄」"} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("system prompt must contain %q", want)
		}
	}
}

// TestAnswerWithEditTriedHint [F34-03] 規格 #18：EditTried 時使用者 prompt 有【系統備註】，System 仍等於 systemPrompt.
func TestAnswerWithEditTriedHint(t *testing.T) {
	c := &fakeCompleter{answers: []string{"好的"}}
	a, _ := newTestAssistant(c, false)
	if _, err := a.AnswerWith(context.Background(), "U1", "把牛肉三明治改為早餐", Hints{EditTried: true}); err != nil {
		t.Fatal(err)
	}
	req := c.reqs[0]
	if !strings.Contains(req.Prompt, "【系統備註】") || !strings.Contains(req.Prompt, "沒有產生修改確認卡") {
		t.Errorf("prompt must carry the system note:\n%s", req.Prompt)
	}
	if req.System != systemPrompt || strings.Contains(req.System, "【系統備註】") {
		t.Error("the note belongs in the user prompt, System must stay equal to systemPrompt")
	}
	if i, j := strings.Index(req.Prompt, "【系統備註】"), strings.Index(req.Prompt, "【問題】"); i < 0 || j < i {
		t.Errorf("note must sit before 【問題】 (note=%d question=%d)", i, j)
	}
	for name, call := range map[string]func(a *Assistant) error{
		"Answer": func(a *Assistant) error { _, err := a.Answer(context.Background(), "U1", "你好"); return err },
		"Hints{}": func(a *Assistant) error {
			_, err := a.AnswerWith(context.Background(), "U1", "你好", Hints{})
			return err
		},
		"EditFalse": func(a *Assistant) error {
			_, err := a.AnswerWith(context.Background(), "U1", "你好", Hints{EditTried: false})
			return err
		},
	} {
		c2 := &fakeCompleter{answers: []string{"好的"}}
		a2, _ := newTestAssistant(c2, false)
		if err := call(a2); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(c2.reqs[0].Prompt, "【系統備註】") {
			t.Errorf("%s must not add the system note:\n%s", name, c2.reqs[0].Prompt)
		}
	}
}

// TestEditTriedFilteredToNoticeBecomesFeatureList [F34-05] 決策 04af8cf4：規劃器試過、模型整段是謊話 → 固定句.
func TestEditTriedFilteredToNoticeBecomesFeatureList(t *testing.T) {
	c := &fakeCompleter{answers: []string{prodLie3}}
	a, h := newTestAssistant(c, false)
	got, err := a.AnswerWith(context.Background(), "U1", "確認", Hints{EditTried: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != editableListReply {
		t.Fatalf("reply = %q, want the fixed editableListReply", got.Text)
	}
	if turns := h.turns["U1"]; turns[len(turns)-1].Answer != editableListReply {
		t.Errorf("history must store the fixed reply, got %q", turns[len(turns)-1].Answer)
	}
	if !strings.Contains(editableListReply, "這次沒有改到任何紀錄") {
		t.Error("fixed reply must say nothing was changed")
	}
	if !strings.Contains(editableListReply, "把午餐的牛肉麵改成 500 大卡") {
		t.Error("fixed reply must carry the one example")
	}
	for _, f := range editableFeatures {
		if !strings.Contains(editableListReply, f.Text) {
			t.Errorf("fixed reply must list %q", f.Text)
		}
	}
	if out, n := filterRecordClaims(editableListReply); n != 0 || out != editableListReply {
		t.Errorf("fixed reply must survive the filter: removed=%d out=%q", n, out)
	}
	if n := utf8.RuneCountInString(editableListReply); n > maxAnswerRunes {
		t.Errorf("fixed reply = %d runes, limit %d", n, maxAnswerRunes)
	}
	// 對照：沒有 EditTried 時維持 qaHonestyNotice，不換成固定句.
	c2 := &fakeCompleter{answers: []string{prodLie3}}
	a2, _ := newTestAssistant(c2, false)
	if got2, err := a2.Answer(context.Background(), "U1", "確認"); err != nil || got2.Text != qaHonestyNotice {
		t.Errorf("without EditTried the reply must stay qaHonestyNotice: %+v err=%v", got2, err)
	}
}

// TestEditTriedWithRealAnswerKeepsModelText [F34-05] 有真內容時保留真內容、附 notice，不換固定句.
func TestEditTriedWithRealAnswerKeepsModelText(t *testing.T) {
	c := &fakeCompleter{answers: []string{"牛肉麵大約 550 大卡。" + prodLie3}}
	a, _ := newTestAssistant(c, false)
	got, err := a.AnswerWith(context.Background(), "U1", "牛肉麵多少大卡，順便確認", Hints{EditTried: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text == editableListReply || !strings.HasPrefix(got.Text, "牛肉麵大約 550 大卡。") || !strings.HasSuffix(got.Text, qaHonestyNotice) {
		t.Errorf("must keep the real content and append the notice: %q", got.Text)
	}
}

// TestEditableFeaturesListOnlyMergedOps [F34-01][F34-10] 清單恰好列出 recordedit/contract.go 除 OpNone 外的十二個 op，每個一次.
func TestEditableFeaturesListOnlyMergedOps(t *testing.T) {
	merged := []string{
		recordedit.OpSetTotalKcal, recordedit.OpSetItemKcal, recordedit.OpSetItemByWeight, recordedit.OpScale,
		recordedit.OpAddItem, recordedit.OpRemoveItem, recordedit.OpRenameItem, recordedit.OpDeleteMeal,
		recordedit.OpSetWorkout, recordedit.OpDeleteWorkout, recordedit.OpSetMealType, recordedit.OpMoveItems,
	}
	if len(editableFeatures) != len(merged) {
		t.Fatalf("editableFeatures has %d entries, want %d", len(editableFeatures), len(merged))
	}
	seen := map[string]bool{}
	for _, f := range editableFeatures {
		if !slices.Contains(merged, f.Op) {
			t.Errorf("op %q is not a merged recordedit op", f.Op)
		}
		if seen[f.Op] {
			t.Errorf("op %q listed twice", f.Op)
		}
		seen[f.Op] = true
		if strings.TrimSpace(f.Text) == "" {
			t.Errorf("op %q has empty text", f.Op)
		}
	}
	for _, op := range merged {
		if !seen[op] {
			t.Errorf("merged op %q missing from editableFeatures", op)
		}
	}
	// [F34-10] 改餐別、移品項、拆多餐已合併：提示詞與固定句都要列出來，客人問得到.
	for _, s := range []string{systemPrompt, editableListReply} {
		if !strings.Contains(s, "改餐別") || !strings.Contains(s, "拆成多餐") {
			t.Errorf("prompt and fixed reply must list the regroup features: %q", s)
		}
	}
}

// TestRule3AsksToReplyGai [F34-06] 規則 3 尾句改成請客人回「改」，不再承諾回「要」就出確認.
func TestRule3AsksToReplyGai(t *testing.T) {
	if !strings.Contains(systemPrompt, "要改的話回「改」") {
		t.Error("rule 3 must ask the customer to reply 「改」")
	}
	if strings.Contains(systemPrompt, "他回「要」") {
		t.Error("rule 3 must no longer promise that replying 「要」 produces a confirmation")
	}
	if !strings.Contains(editSystemPrompt, "「要」「好」「可以」「對」") {
		t.Error("planner rule 3 must accept 要/好/可以/對 as adopting the earlier estimate")
	}
}

// TestLastTurnOfferedEdit [F34-07] 提議句判定表（預期值取自開工單，不取自實作）.
func TestLastTurnOfferedEdit(t *testing.T) {
	cases := []struct {
		name   string
		answer string
		want   bool
	}{
		{"正：要不要幫你把這一筆改成這個數字", "我估算約 210 大卡，要不要幫你把這一筆改成這個數字？", true},
		{"正：要改的話回「改」", "紀錄是 150 大卡，依你的描述估算約 210 大卡。要改的話回「改」。", true},
		{"正：要不要把午餐的麵包改成 210 大卡", "要不要把午餐的麵包改成 210 大卡？", true},
		{"反：editClaimNotice", editClaimNotice, false},
		{"反：editableListReply", editableListReply, false},
		{"反：editOfferConsumedNote", editOfferConsumedNote, false},
		{"反：一般回答", "牛肉麵大約 550 大卡。", false},
		{"反：要不要再吃點蔬菜", "要不要再吃點蔬菜？", false},
		{"反：把晚餐改成清淡一點", "你可以把晚餐改成清淡一點。", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, h := newTestAssistant(&fakeCompleter{}, false)
			h.turns["U1"] = []Turn{{Question: "這個多少大卡", Answer: tc.answer}}
			if got := a.LastTurnOfferedEdit(context.Background(), "U1"); got != tc.want {
				t.Errorf("LastTurnOfferedEdit(%q) = %v, want %v", tc.answer, got, tc.want)
			}
		})
	}
	t.Run("反：提議在倒數第二輪，最後一輪是一般回答", func(t *testing.T) {
		a, h := newTestAssistant(&fakeCompleter{}, false)
		h.turns["U1"] = []Turn{
			{Question: "q1", Answer: "我估算約 210 大卡，要不要幫你把這一筆改成這個數字？"},
			{Question: "q2", Answer: "牛肉麵大約 550 大卡。"},
		}
		if a.LastTurnOfferedEdit(context.Background(), "U1") {
			t.Error("only the last turn counts")
		}
	})
	t.Run("反：沒有 history", func(t *testing.T) {
		a, _ := newTestAssistant(&fakeCompleter{}, false)
		if a.LastTurnOfferedEdit(context.Background(), "U1") {
			t.Error("empty history is not an offer")
		}
	})
	t.Run("反：他人的 history 含提議句", func(t *testing.T) {
		a, h := newTestAssistant(&fakeCompleter{}, false)
		h.turns["U2"] = []Turn{{Question: "q", Answer: "要不要把午餐的麵包改成 210 大卡？"}}
		if a.LastTurnOfferedEdit(context.Background(), "U1") {
			t.Error("another user's offer must not count")
		}
		if !a.LastTurnOfferedEdit(context.Background(), "U2") {
			t.Error("control: U2's own offer must count")
		}
	})
	t.Run("nil Assistant", func(t *testing.T) {
		var a *Assistant
		if a.LastTurnOfferedEdit(context.Background(), "U1") {
			t.Error("nil assistant is not an offer")
		}
	})
}

// TestConsumeEditOffer [F34-07] 消耗後最後一輪不再是提議，但提議句仍在規劃器看得到的視窗內.
func TestConsumeEditOffer(t *testing.T) {
	const offer = "我估算約 210 大卡，要不要幫你把這一筆改成這個數字？"
	ctx := context.Background()
	for name, turns := range map[string][]Turn{
		"history 未滿": {{Question: "q0", Answer: "a0"}, {Question: "這個多少", Answer: offer}},
		"history 已滿": {{Question: "q0", Answer: "a0"}, {Question: "q1", Answer: "a1"}, {Question: "這個多少", Answer: offer}},
	} {
		t.Run(name, func(t *testing.T) {
			a, h := newTestAssistant(&fakeCompleter{}, false)
			h.turns["U1"] = turns
			a.ConsumeEditOffer(ctx, "U1", "要")
			if a.LastTurnOfferedEdit(ctx, "U1") {
				t.Error("after consuming, the last turn must no longer be an offer")
			}
			got := h.turns["U1"]
			last := got[len(got)-1]
			if last.Question != "要" || last.Answer != editOfferConsumedNote {
				t.Errorf("last turn = %+v", last)
			}
			if got[len(got)-2].Answer != offer {
				t.Errorf("the offer must stay the second-to-last turn (planner window), got %q", got[len(got)-2].Answer)
			}
			// 再消耗一次：最後一輪已不是提議，不得多寫一輪.
			n := len(got)
			a.ConsumeEditOffer(ctx, "U1", "好")
			if len(h.turns["U1"]) != n {
				t.Errorf("a second consume must not append (len %d -> %d)", n, len(h.turns["U1"]))
			}
		})
	}
	t.Run("不是提議就不寫", func(t *testing.T) {
		a, h := newTestAssistant(&fakeCompleter{}, false)
		h.turns["U1"] = []Turn{{Question: "q", Answer: "牛肉麵大約 550 大卡。"}}
		a.ConsumeEditOffer(ctx, "U1", "好")
		if len(h.turns["U1"]) != 1 {
			t.Errorf("history must be untouched: %+v", h.turns["U1"])
		}
	})
	t.Run("nil Assistant 不 panic", func(t *testing.T) {
		var a *Assistant
		a.ConsumeEditOffer(ctx, "U1", "要")
	})
}
