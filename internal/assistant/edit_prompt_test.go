package assistant

import (
	"strings"
	"testing"
)

// [F33-16] A32 規則 9 不再叫客人刪掉重記，也不留舊的「重新記錄訊息」教學；「不得說已更新」等既有禁令仍在.
func TestSystemPromptNoDeleteAndRerecordAdvice(t *testing.T) {
	for _, banned := range []string{"刪掉再重新", "把那一筆刪掉", "我的統計", "重新記錄訊息", "可以直接複製"} {
		if strings.Contains(systemPrompt, banned) {
			t.Errorf("system prompt must not contain %q", banned)
		}
	}
	for _, want := range []string{
		"你自己不能改紀錄", "系統會出確認按鈕", "按下確認才會寫入", "你不要說已經改好",
		"不得說「已更新」", "不得說「已記錄」", "也不得提到沒有列在這份規則裡的按鈕或功能",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("system prompt must keep %q", want)
		}
	}
}

// [F33-16] A32 editClaimNotice 新文案指向直接修改、不含刪除重記、不含任何宣稱型態（過濾冪等）；規則 3 結尾改成問客人要不要改.
func TestEditClaimNoticeAndRule3PointToDirectEdit(t *testing.T) {
	const want = "要修改紀錄，直接告訴我要改哪一筆、改成多少，我會請你確認。"
	if editClaimNotice != want {
		t.Errorf("editClaimNotice = %q, want %q", editClaimNotice, want)
	}
	if strings.Contains(editClaimNotice, "刪掉") || strings.Contains(editClaimNotice, "重新記錄") {
		t.Errorf("notice must not tell the user to delete and re-record: %q", editClaimNotice)
	}
	if isEditClaim(editClaimNotice) || isRecordClaim(editClaimNotice) {
		t.Errorf("notice must not look like a claim: %q", editClaimNotice)
	}
	if !strings.Contains(systemPrompt, "就由系統出修改確認") {
		t.Error("rule 3 must hand the actual edit to the system confirmation")
	}
	if strings.Contains(systemPrompt, "接著照規則 9 告訴他怎麼把紀錄改成新的數字") {
		t.Error("rule 3 must no longer point to the delete-and-rerecord rule 9")
	}
	// 補上的指引在過濾後仍然冪等，且謊稱句照樣被濾掉（cb025c5 不退化）.
	got, n := filterRecordClaims("已將今天的熱量紀錄更新為 700 大卡。你今天已記錄 2 餐。")
	if n != 1 || !strings.Contains(got, editClaimNotice) || strings.Contains(got, "已將") {
		t.Errorf("filter result = %q (removed %d)", got, n)
	}
	if again, m := filterRecordClaims(got); m != 0 || again != got {
		t.Errorf("filter must be idempotent: %q (%d)", again, m)
	}
}
