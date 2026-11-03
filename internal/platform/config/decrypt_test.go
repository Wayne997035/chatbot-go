package config

import (
	"chatbot-go/internal/crypto"
	"slices"
	"strings"
	"testing"
)

func enc(t *testing.T, plaintext, keyset string) string {
	t.Helper()
	ct, err := crypto.Encrypt(plaintext, keyset)
	if err != nil {
		t.Fatal(err)
	}
	return "ENC(" + ct + ")"
}

// 金鑰輪替中：舊金鑰加密的值照樣解得開，而且會列出來提醒重新加密；管理 token 也可以加密.
func TestDecryptDuringKeyRotation(t *testing.T) {
	oldKS, err := crypto.NewKeySet()
	if err != nil {
		t.Fatal(err)
	}
	secretUnderOldKey := enc(t, "line-secret-value", oldKS)
	newKS, _, err := crypto.RotateKeySet(oldKS)
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{
		Line:     LineConfig{ChannelSecret: secretUnderOldKey, ChannelToken: enc(t, "line-token-value", newKS), ReplyURL: "plain"},
		Security: SecurityConfig{KeySet: newKS, AdminToken: enc(t, "admin-token-value", newKS)},
	}

	r, err := decryptConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	if c.Line.ChannelSecret != "line-secret-value" || c.Line.ChannelToken != "line-token-value" ||
		c.Security.AdminToken != "admin-token-value" || c.Line.ReplyURL != "plain" || c.Security.KeySet != newKS {
		t.Errorf("decrypted config = %+v", c)
	}
	if !slices.Equal(r.decrypted, []string{"line.channel_secret", "line.channel_token", "security.admin_token"}) {
		t.Errorf("decrypted fields = %v", r.decrypted)
	}
	if !slices.Equal(r.oldKey, []string{"line.channel_secret"}) {
		t.Errorf("fields still under the old key = %v", r.oldKey)
	}
}

// 錯誤只說是哪個欄位，不含值；沒設金鑰集、用錯金鑰都要停止啟動.
func TestDecryptErrorsNameTheFieldButNoValues(t *testing.T) {
	ks, _ := crypto.NewKeySet()
	otherKS, _ := crypto.NewKeySet()
	value := enc(t, "mongodb://user:pass@host/db", ks)

	_, err := decryptConfig(&Config{Database: DatabaseConfig{Mongo: MongoConfig{URI: value}}})
	if err == nil || !strings.Contains(err.Error(), "database.mongo.uri") || !strings.Contains(err.Error(), "CRYPTO_KEYSET") {
		t.Errorf("missing keyset: %v", err)
	}

	_, err = decryptConfig(&Config{Database: DatabaseConfig{Mongo: MongoConfig{URI: value}}, Security: SecurityConfig{KeySet: otherKS}})
	if err == nil || !strings.Contains(err.Error(), "database.mongo.uri") {
		t.Errorf("wrong keyset: %v", err)
	}
	for _, leak := range []string{"pass", value[4:20], otherKS[:20]} {
		if err != nil && strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}
}
