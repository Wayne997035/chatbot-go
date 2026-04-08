package crypto

import (
	"fmt"
	"strings"
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

func TestManualEncrypt(t *testing.T) {
	// 手動用途：將keyset換成你的key 把 inputs 改成你要加密的值，然後執行：
	// go test -run TestManualEncrypt -v ./internal/crypto
	// 測試會輸出 ENC(...)，並驗證解密可還原。
	keyset := mustKeySet(t, 10)

	inputs := []struct {
		name string
		val  string
	}{
		{name: "database.mongo.uri", val: "mongodb+srv://{account}:{password}@{cluster}/{database}"},
		{name: "database.redis.host", val: "{redisHost}"},
		{name: "database.redis.password", val: "{redisPassword}"},
	}

	t.Log("===== Keyset (for local.yaml security.keyset) =====")
	t.Logf("keyset=%s", keyset)
	t.Log("===== Encrypted values (paste into local.yaml) =====")

	for _, in := range inputs {
		ct, err := Encrypt(in.val, keyset)
		if err != nil {
			t.Fatalf("encrypt %s: %v", in.name, err)
		}
		enc := "ENC(" + ct + ")"
		t.Logf("%s: %s", in.name, enc)

		// 驗證：確認用同一把 keyset 可以解密還原
		inner := strings.TrimSuffix(strings.TrimPrefix(enc, "ENC("), ")")
		plain, err := Decrypt(inner, keyset)
		if err != nil {
			t.Fatalf("decrypt %s: %v", in.name, err)
		}
		if plain != in.val {
			t.Fatalf("%s mismatch: got %q, want %q", in.name, plain, in.val)
		}
	}
}
