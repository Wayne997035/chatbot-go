package assistant

import (
	"chatbot-go/internal/ai"
	"chatbot-go/internal/assistant/recordedit"
	"chatbot-go/internal/storage/database/meal"
	"context"
	"strings"
	"testing"
	"time"
)

// seqCompleter 第 n 次呼叫只回第 n 個答案（照 Accept 判定）；沒有答案或答案不被接受就回失敗.
type seqCompleter struct {
	answers []string
	reqs    []*ai.Request
}

func (s *seqCompleter) Complete(_ context.Context, req *ai.Request) ai.Result {
	s.reqs = append(s.reqs, req)
	i := len(s.reqs) - 1
	if i < len(s.answers) && (req.Accept == nil || req.Accept(s.answers[i])) {
		return ai.Result{Text: s.answers[i], Provider: "groq", Model: "qwen/qwen3.8-27b"}
	}
	return ai.Result{Errors: []string{"groq(m) 429: rate limited"}}
}

// [F33-44] 決策 2805c816 ①：op none 卻列了 ≥2 個候選 → 附提醒重問一次；
// 重問給出有效計畫就照它走，仍 none 或失敗就交回問答；沒有候選不重問.
func TestPlanEditRetriesOnceWhenNoneListsCandidates(t *testing.T) {
	meals := []meal.Meal{bread("ma", "U1", 120, time.Hour, 1), bread("mb", "U1", 300, 3*time.Hour, 1)}
	const noneWithCands = `{"op":"none","candidates":["M1","M2"]}`
	cases := []struct {
		name    string
		answers []string
		kind    recordedit.OutcomeKind
		calls   int
	}{
		{"retry gives op with candidates", []string{noneWithCands, `{"op":"set_item_kcal","item":1,"kcal":150,"candidates":["M1","M2"]}`}, recordedit.OutcomeClarify, 2},
		{"retry gives op with target", []string{noneWithCands, `{"op":"set_item_kcal","target":"M2","item":1,"kcal":450}`}, recordedit.OutcomeProposal, 2},
		{"none twice", []string{noneWithCands, noneWithCands}, recordedit.OutcomeNone, 2},
		{"retry unusable", []string{noneWithCands, "我不確定"}, recordedit.OutcomeNone, 2},
		{"none without candidates", []string{`{"op":"none"}`, setItem210}, recordedit.OutcomeNone, 1},
		{"none with one candidate", []string{`{"op":"none","candidates":["M1"]}`, setItem210}, recordedit.OutcomeNone, 1},
		{"valid first plan", []string{setItem210, setItem210}, recordedit.OutcomeProposal, 1},
	}
	for _, tc := range cases {
		seq := &seqCompleter{answers: tc.answers}
		a, rm := plannerAssistant(&fakeCompleter{}, meals, nil, false)
		a.completer = seq
		out, err := a.PlanEdit(context.Background(), "U1", "麵包幫我改成 150")
		if err != nil || out.Kind != tc.kind || len(seq.reqs) != tc.calls {
			t.Errorf("%s: kind=%d calls=%d err=%v, want kind %d calls %d", tc.name, out.Kind, len(seq.reqs), err, tc.kind, tc.calls)
			continue
		}
		if rm.writes != 0 {
			t.Errorf("%s: planner must not write", tc.name)
		}
		if tc.calls == 2 {
			first, second := seq.reqs[0].Prompt, seq.reqs[1].Prompt
			if strings.Contains(first, editRetryNote) || second != first+editRetryNote {
				t.Errorf("%s: retry prompt must be the first prompt plus editRetryNote", tc.name)
			}
		}
		switch tc.name {
		case "retry gives op with candidates":
			if out.Plan == nil || out.Plan.Op != recordedit.OpSetItemKcal || out.Plan.Kcal != 150 || len(out.Candidates) != 2 ||
				out.Candidates[0].RecordID != "ma" || out.Candidates[1].RecordID != "mb" {
				t.Errorf("%s: clarify = %+v", tc.name, out)
			}
		case "retry gives op with target":
			if p := out.Proposal; p == nil || p.RecordID != "mb" || p.Before != 300 || p.After != 450 {
				t.Errorf("%s: proposal = %+v", tc.name, out.Proposal)
			}
		}
	}
}
