package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func TestAESGCM_RoundTrip(t *testing.T) {
	// 產生 AES-128 key
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keyBase64 := base64.StdEncoding.EncodeToString(key)

	tests := []struct {
		name      string
		plaintext string
	}{
		{"english", "hello world"},
		{"chinese", "這是一段中文測試"},
		{"empty", ""},
		{"special chars", "!@#$%^&*()_+-={}[]|\\:\";<>?,./~`"},
		{"long text", "a very long string that exceeds typical block sizes for AES encryption testing purposes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encrypted, err := EncryptAESGCM(tt.plaintext, keyBase64)
			if err != nil {
				t.Fatalf("encrypt: %v", err)
			}

			if encrypted == tt.plaintext && tt.plaintext != "" {
				t.Error("encrypted should differ from plaintext")
			}

			decrypted, err := DecryptAESGCM(encrypted, keyBase64)
			if err != nil {
				t.Fatalf("decrypt: %v", err)
			}

			if decrypted != tt.plaintext {
				t.Errorf("got %q, want %q", decrypted, tt.plaintext)
			}
		})
	}
}

func TestAESGCM_InvalidKey(t *testing.T) {
	_, err := EncryptAESGCM("test", "not-valid-base64!!!")
	if err == nil {
		t.Error("should fail with invalid key")
	}
}

func TestAESGCM_WrongKey(t *testing.T) {
	key1 := make([]byte, 16)
	key2 := make([]byte, 16)
	rand.Read(key1)
	rand.Read(key2)

	encrypted, err := EncryptAESGCM("secret", base64.StdEncoding.EncodeToString(key1))
	if err != nil {
		t.Fatal(err)
	}

	_, err = DecryptAESGCM(encrypted, base64.StdEncoding.EncodeToString(key2))
	if err == nil {
		t.Error("should fail with wrong key")
	}
}
