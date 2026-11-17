package assistant

import (
	"strings"
	"testing"
)

// TestFinalizeKeepsEstimateWithUnicodeSpaces [F33-38] 決策 91563b18 ①：模型（Groq gpt-oss）常在數字與單位之間放
// 窄空白 U+202F；Go regexp 的 \s 只認 ASCII 空白，沒正規化時重新估算句會被當成沒有數字而整句刪掉（#19 復發）.
func TestFinalizeKeepsEstimateWithUnicodeSpaces(t *testing.T) {
	for _, sp := range []rune{' ', ' ', ' ', '　'} {
		s := "依新資訊把熱量修正為約 250" + string(sp) + "大卡。"
		final, _, removed := finalize(s)
		if removed != 0 || !strings.Contains(final, "250 大卡") || strings.Count(final, editClaimNotice) != 1 {
			t.Errorf("space %U: estimate must be kept with one notice, got %q (removed %d)", sp, final, removed)
		}
	}

	// 2026-11-17 真模型重播原文（groq openai/gpt-oss-120b），數字與單位、範圍符號兩側都是 U+202F.
	const replay = "黑麥麵包每 100 克約 130 大卡，計算後約 105 ~ 117 大卡，與你紀錄的 120 大卡相近。"
	final, _, _ := finalize(replay)
	if strings.ContainsRune(final, ' ') || !strings.Contains(final, "105 ~ 117 大卡") {
		t.Errorf("unicode spaces must become plain spaces: %q", final)
	}

	// 反向：提到紀錄的謊稱句換成窄空白照樣刪.
	const lie = "已將今天的熱量紀錄更新為 700 大卡。"
	if got, _, removed := finalize(lie); removed != 1 || strings.Contains(got, "700 大卡") {
		t.Errorf("claim with a narrow space must still be removed: %q (removed %d)", got, removed)
	}
}
