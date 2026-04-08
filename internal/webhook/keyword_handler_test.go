package webhook

import (
	"strings"
	"testing"
)

func TestIsValidRegion(t *testing.T) {
	tests := []struct {
		region string
		want   bool
	}{
		{"台北市", true},
		{"新北市", true},
		{"高雄市", true},
		{"花蓮縣", true},
		{"連江縣", true},
		{"臺北市", false}, // traditional character not normalized
		{"", false},
		{"福建省", false},
		{"台北", false}, // partial
	}

	for _, tt := range tests {
		t.Run(tt.region, func(t *testing.T) {
			if got := isValidRegion(tt.region); got != tt.want {
				t.Errorf("isValidRegion(%q) = %v, want %v", tt.region, got, tt.want)
			}
		})
	}
}

func TestTaiwanRegionsCount(t *testing.T) {
	if len(taiwanRegions) != 22 {
		t.Errorf("expected 22 regions, got %d", len(taiwanRegions))
	}
}

func TestIsFinishKeyword(t *testing.T) {
	finishWords := []string{"完成", "好了", "不用了", "結束"}
	nonFinishWords := []string{"台北市", "", "done", "cancel"}

	for _, kw := range finishWords {
		t.Run("finish/"+kw, func(t *testing.T) {
			if !isFinishKeyword(kw) {
				t.Errorf("isFinishKeyword(%q) = false, want true", kw)
			}
		})
	}

	for _, kw := range nonFinishWords {
		t.Run("non-finish/"+kw, func(t *testing.T) {
			if isFinishKeyword(kw) {
				t.Errorf("isFinishKeyword(%q) = true, want false", kw)
			}
		})
	}
}

func TestRegionListMessage(t *testing.T) {
	msg := regionListMessage()
	if msg == "" {
		t.Error("expected non-empty region list message")
	}
	if !strings.Contains(msg, "台北市") {
		t.Error("expected region list message to contain 台北市")
	}
}
