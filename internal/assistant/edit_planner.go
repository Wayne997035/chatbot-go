package assistant

import (
	"chatbot-go/internal/assistant/recordedit"
	"context"
)

// 編譯期確認 *Assistant 滿足 webhook 依賴的 recordedit.Planner 介面.
var _ recordedit.Planner = (*Assistant)(nil)

// PlanEdit 把客人「要改什麼、刪什麼」的訊息轉成結構化修改計畫（契約空殼：一律回 OutcomeNone，
// 訊息照舊交給問答）。實作在規格 .specs/reanalyze/chat-edit.md §4-§5.
func (a *Assistant) PlanEdit(_ context.Context, _, _ string) (*recordedit.Outcome, error) {
	return &recordedit.Outcome{Kind: recordedit.OutcomeNone}, nil
}
