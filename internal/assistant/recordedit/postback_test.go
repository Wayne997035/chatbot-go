package recordedit

import (
	"strings"
	"testing"
)

// [F33-14] A18 ParsePostback：只認 ce1:ok|no:<nonce>、ce1:pick:<nonce>:<0-4>；其餘一律 ok=false.
func TestParsePostbackTable(t *testing.T) {
	nonce, err := NewNonce()
	if err != nil || !ValidNonce(nonce) {
		t.Fatalf("NewNonce = %q, %v", nonce, err)
	}
	n2 := strings.Repeat("a", NonceLen)
	cases := []struct {
		name string
		data string
		want Postback
		ok   bool
	}{
		{"ok", "ce1:ok:" + n2, Postback{Action: ActionOK, Nonce: n2}, true},
		{"cancel", "ce1:no:" + n2, Postback{Action: ActionCancel, Nonce: n2}, true},
		{"pick 0", "ce1:pick:" + n2 + ":0", Postback{Action: ActionPick, Nonce: n2, Index: 0}, true},
		{"pick 4", "ce1:pick:" + n2 + ":4", Postback{Action: ActionPick, Nonce: n2, Index: 4}, true},
		{"pick 5", "ce1:pick:" + n2 + ":5", Postback{}, false},
		{"pick -1", "ce1:pick:" + n2 + ":-1", Postback{}, false},
		{"pick abc", "ce1:pick:" + n2 + ":abc", Postback{}, false},
		{"pick two digits", "ce1:pick:" + n2 + ":01", Postback{}, false},
		{"pick without index", "ce1:pick:" + n2, Postback{}, false},
		{"ok with extra part", "ce1:ok:" + n2 + ":1", Postback{}, false},
		{"unknown action", "ce1:del:" + n2, Postback{}, false},
		{"wrong prefix", "ce2:ok:" + n2, Postback{}, false},
		{"short nonce", "ce1:ok:abc", Postback{}, false},
		{"nonce with bad char", "ce1:ok:" + strings.Repeat("a", NonceLen-1) + "!", Postback{}, false},
		{"over 64 bytes", "ce1:ok:" + n2 + strings.Repeat("x", 40), Postback{}, false},
		{"empty", "", Postback{}, false},
		{"record id smuggled", "ce1:ok:" + n2 + ":507f1f77bcf86cd799439011", Postback{}, false},
	}
	for _, c := range cases {
		got, ok := ParsePostback(c.data)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%s: ParsePostback(%q) = %+v,%v want %+v,%v", c.name, c.data, got, ok, c.want, c.ok)
		}
	}
	if d := PostbackData(ActionPick, n2, 3); d != "ce1:pick:"+n2+":3" {
		t.Errorf("PostbackData = %q", d)
	}
	if len(PostbackData(ActionPick, n2, 4)) > 64 {
		t.Error("postback data must stay within 64 bytes")
	}
}
