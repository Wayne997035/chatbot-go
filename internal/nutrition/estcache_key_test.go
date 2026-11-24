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
		{"spaces and fullwidth punct", "珍珠奶茶", " 珍珠 奶茶！", true},
		{"case and fullwidth ascii", "Latte 大杯", "ＬＡＴＴＥ大杯", true},
		{"decimal point kept", "1.5碗麵", "15碗麵", false},
		{"slash kept", "1/2碗飯", "12碗飯", false},
		{"percent kept", "50%糖", "50糖", false},
		{"plus kept", "蛋餅+豆漿", "蛋餅豆漿", false},
		{"sugar level", "半糖", "全糖", false},
		{"meal word is part of text", "早餐蛋餅", "蛋餅", false},
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
		t.Fatal("TextFingerprint behaviour changed; it must stay as is")
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
