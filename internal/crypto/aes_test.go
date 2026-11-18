package crypto

import (
	"fmt"
	"sync"
	"testing"
)

func mustKeySet(t *testing.T, n int) string {
	t.Helper()
	ks, err := GenerateKeySet(n)
	if err != nil {
		t.Fatalf("GenerateKeySet(%d): %v", n, err)
	}
	return ks
}

func TestTinkAEAD_RoundTrip(t *testing.T) {
	keyset := mustKeySet(t, 3)

	tests := []struct {
		name      string
		plaintext string
	}{
		{name: "english", plaintext: "hello world"},
		{name: "chinese", plaintext: "這是一段中文測試"},
		{name: "empty", plaintext: ""},
		{name: "special chars", plaintext: "!@#$%^&*()_+-={}[]|\\:\";<>?,./~`"},
		{name: "long text", plaintext: "a very long string that exceeds typical block sizes for encryption testing purposes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encrypted, err := Encrypt(tt.plaintext, keyset)
			if err != nil {
				t.Fatalf("encrypt: %v", err)
			}

			if encrypted == tt.plaintext && tt.plaintext != "" {
				t.Error("encrypted should differ from plaintext")
			}

			decrypted, err := Decrypt(encrypted, keyset)
			if err != nil {
				t.Fatalf("decrypt: %v", err)
			}

			if decrypted != tt.plaintext {
				t.Errorf("got %q, want %q", decrypted, tt.plaintext)
			}
		})
	}
}

func TestTinkAEAD_InvalidKeySet(t *testing.T) {
	_, err := Encrypt("test", "not-valid-base64!!!")
	if err == nil {
		t.Error("should fail with invalid keyset")
	}
}

func TestTinkAEAD_WrongKeySet(t *testing.T) {
	keyset1 := mustKeySet(t, 2)
	keyset2 := mustKeySet(t, 2)

	encrypted, err := Encrypt("secret", keyset1)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Decrypt(encrypted, keyset2)
	if err == nil {
		t.Error("should fail with wrong keyset")
	}
}

func TestTinkAEAD_ConcurrentKeySets(t *testing.T) {
	const numKeySets = 10
	const numRoutines = 1000

	keysets := make([]string, numKeySets)
	for i := 0; i < numKeySets; i++ {
		keysets[i] = mustKeySet(t, 2)
	}

	var wg sync.WaitGroup
	wg.Add(numRoutines)

	for i := 0; i < numRoutines; i++ {
		go func(routineID int) {
			defer wg.Done()

			keyset := keysets[routineID%numKeySets]
			plaintext := fmt.Sprintf("concurrent test data %d", routineID)

			encrypted, err := Encrypt(plaintext, keyset)
			if err != nil {
				t.Errorf("encrypt error: %v", err)
				return
			}

			decrypted, err := Decrypt(encrypted, keyset)
			if err != nil {
				t.Errorf("decrypt error: %v", err)
				return
			}

			if decrypted != plaintext {
				t.Errorf("got %q, want %q", decrypted, plaintext)
			}
		}(i)
	}

	wg.Wait()
}

func TestGenerateKeySet_InvalidCount(t *testing.T) {
	_, err := GenerateKeySet(0)
	if err == nil {
		t.Fatal("expected error when key count < 1")
	}
}

// [F34-40] 原本在這裡的手動加密測試會把 keyset 與 ENC 值印進 CI 公開 log，已刪除；加密改走 cmd/secrets.
