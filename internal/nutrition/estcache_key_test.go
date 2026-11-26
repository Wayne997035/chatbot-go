package nutrition

import (
	"strings"
	"testing"
)

// [F37-41] 正規化：同一段文字的寫法差異同 key，數字與份量差異不同 key.
func TestNormalizeMealText(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		same bool
	}{
		{"edge spaces and fullwidth punct", "珍珠奶茶", " 珍珠奶茶！ ", true},
		{"edge brackets", "「珍珠奶茶」", "珍珠奶茶", true},
		{"case and fullwidth ascii", "Latte 大杯", "ＬＡＴＴＥ　大杯", true},
		{"space runs collapse", "大杯  珍珠奶茶", "大杯 珍珠奶茶", true},
		{"decimal point kept", "1.5碗麵", "15碗麵", false},
		{"slash kept", "1/2碗飯", "12碗飯", false},
		{"percent kept", "50%糖", "50糖", false},
		{"plus kept", "蛋餅+豆漿", "蛋餅豆漿", false},
		{"sugar level", "半糖", "全糖", false},
		{"meal word is part of text", "早餐蛋餅", "蛋餅", false},
		// [PR37 R1-01 / SE37-02] 符號有語意：emoji 不同或有無都不得同 key.
		{"emoji differ", "吃了🍔", "吃了🍕", false},
		{"emoji present vs absent", "午餐吃了牛肉麵🍜🍜🍜", "午餐吃了牛肉麵", false},
		{"circled letter kept", "套餐ⓐ", "套餐ⓑ", false},
		// 夾在數字之間的空白或標點不得讓兩個數字黏成一個.
		{"hyphen between digits", "1-2碗", "12碗", false},
		{"tilde between digits", "1~2碗", "12碗", false},
		{"space between digits", "1 2碗", "12碗", false},
		{"comma between digits", "1,5碗", "15碗", false},
		{"trailing punct after digit merges", "2碗，", "2碗", true},
		// [PR37 R2-01 / SE37-R2-02] 中間的標點與空白都可能改變意思，一律保留.
		{"comma changes meaning", "便當不，要加飯", "便當不要加飯", false},
		{"comma changes meaning 2", "紅茶不，加糖", "紅茶不加糖", false},
		{"space can stand for a comma", "便當不 要加飯", "便當不要加飯", false},
		{"hyphen vs comma between digits", "1-5顆水餃", "1,5顆水餃", false},
		{"comma vs colon between digits", "1,5顆水餃", "1:5顆水餃", false},
		{"hyphen vs space between digits", "1-2碗", "1 2碗", false},
		{"space before decimal point", "1 .5碗", "1.5碗", false},
		{"space after decimal point", "1. 5碗白飯", "1.5碗白飯", false},
		{"user bar vs hyphen", "1|2碗", "1-2碗", false},
		{"fullwidth space equals space", "1　2碗", "1 2碗", true},
		// [F39-04] 頭尾帶數量或編號語意的標點保留，不得撞 key.
		{"edge inch mark kept", "潛艇堡 6\"", "潛艇堡 6", false},
		{"edge hash kept", "#3 套餐", "3 套餐", false},
		{"edge hyphen kept", "-1碗飯", "1碗飯", false},
		{"edge asterisk kept", "*2 漢堡", "2 漢堡", false},
		// [F39-13] 句首的「,」可能是小數點（,5 = 0.5），保留；句尾照去（見上面 "trailing punct after digit merges"）.
		{"edge leading comma kept", ",5碗飯", "5碗飯", false},
		{"edge leading fullwidth comma kept", "，5碗飯", "5碗飯", false},
		// [F40-08] 句尾「?」（含全形）與接在漢字後的「~」去掉；句首的「?」「~」與接在數字後的「~」保留.
		{"trailing question mark", "雞腿便當？", "雞腿便當", true},
		{"leading question mark kept", "？雞腿便當", "雞腿便當", false},
		{"trailing question run", "雞腿便當???", "雞腿便當", true},
		{"trailing question and bang", "雞腿便當？！", "雞腿便當", true},
		{"middle question mark kept", "雞腿?便當", "雞腿便當", false},
		{"trailing tilde after han", "雞腿便當~", "雞腿便當", true},
		{"trailing fullwidth tilde after han", "雞腿便當～", "雞腿便當", true},
		{"trailing tilde after digit kept", "麵 300~", "麵 300", false},
		{"leading tilde kept", "~雞腿便當", "雞腿便當", false},
		{"fullwidth leading tilde folds to ascii", "～雞腿便當", "~雞腿便當", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ka, kb := currentEstimateKey(true, tt.a), currentEstimateKey(true, tt.b)
			if (ka == kb) != tt.same {
				t.Fatalf("[F37-41] key equality of %q vs %q = %v, want %v (norm %q vs %q)",
					tt.a, tt.b, ka == kb, tt.same, NormalizeMealText(tt.a), NormalizeMealText(tt.b))
			}
		})
	}
	// TextFingerprint 的輸出不得被本線改變：它仍把 1.5 與 15 視為同一段.
	if TextFingerprint("1.5碗麵") != TextFingerprint("15碗麵") {
		t.Fatal("TextFingerprint behavior changed; it must stay as is")
	}
}

func TestEstimateCacheKeyShape(t *testing.T) {
	k := currentEstimateKey(true, "珍珠奶茶")
	parts := strings.Split(k, ":")
	if len(parts) != 5 || parts[0] != "mealest" || parts[1] != "v1" || len(parts[2]) != 8 || parts[3] != "c" || len(parts[4]) != 64 {
		t.Fatalf("[F37-41] unexpected key shape %q", k)
	}
	if !strings.HasPrefix(k, estimateKeyPrefix) {
		t.Fatalf("key %q lacks prefix %q", k, estimateKeyPrefix)
	}
}

func TestEstimateCacheKeyChangesWithPrompt(t *testing.T) {
	before := currentEstimateKey(true, "珍珠奶茶")
	orig := estimatePromptHash
	t.Cleanup(func() { estimatePromptHash = orig })
	estimatePromptHash[0] = promptHash8(textPrompt + "changed")
	if after := currentEstimateKey(true, "珍珠奶茶"); after == before {
		t.Fatal("[F37-41] key must change when the prompt hash changes")
	}
	if estimateKey(estimateSchema+1, orig[0], true, "珍珠奶茶") == before {
		t.Fatal("[F37-41] key must change when the schema is bumped")
	}
}

// [F40-08] 句首「~」（約）原樣保留；句尾「?」與接在漢字後的「~」去掉.
func TestNormalizeMealTextTailPunct(t *testing.T) {
	tests := []struct{ in, want string }{
		{"~雞腿便當", "~雞腿便當"},
		{"～雞腿便當", "~雞腿便當"},
		{"雞腿便當？", "雞腿便當"},
		{"雞腿便當???", "雞腿便當"},
		{"麵 300~", "麵 300~"},
		{"雞腿便當~", "雞腿便當"},
		// [F43-01] 連續的「~」與單一個同 key;數字後的照舊保留,只有「~」的也保留
		{"雞腿便當~~", "雞腿便當"},
		{"雞腿便當～～～", "雞腿便當"},
		{"麵 300~~", "麵 300~~"},
		{"~~", "~~"},
		// [F43-07] 中文數字後的「~」與阿拉伯數字一樣保留(約略或範圍)
		{"水餃十~", "水餃十~"},
		{"水餃十~~", "水餃十~~"},
		{"牛肉麵半~", "牛肉麵半~"},
		// [F44-06] 「幾」「两」「廿」也是數量字
		{"水餃十幾~", "水餃十幾~"},
		{"水餃两~", "水餃两~"},
		{"雞蛋廿~", "雞蛋廿~"},
	}
	for _, tt := range tests {
		if got := NormalizeMealText(tt.in); got != tt.want {
			t.Errorf("[F40-08] NormalizeMealText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
