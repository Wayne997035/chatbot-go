package crypto

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
)

func mustNewKeySet(t *testing.T) string {
	t.Helper()
	ks, err := NewKeySet()
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

func mustEncrypt(t *testing.T, plaintext, ks string) string {
	t.Helper()
	ct, err := Encrypt(plaintext, ks)
	if err != nil {
		t.Fatal(err)
	}
	return ct
}

// 輪替：加入新金鑰並設為主要；舊密文仍可解密，新的加密改用新金鑰.
func TestRotateKeepsOldCiphertextsReadableAndEncryptsWithTheNewKey(t *testing.T) {
	ks := mustNewKeySet(t)
	oldPrimary, _ := PrimaryKeyID(ks)
	oldCT := mustEncrypt(t, "channel-secret", ks)

	rotated, newPrimary, err := RotateKeySet(ks)
	if err != nil || newPrimary == oldPrimary {
		t.Fatalf("rotate: primary %d → %d, err %v", oldPrimary, newPrimary, err)
	}
	if got, err := Decrypt(oldCT, rotated); err != nil || got != "channel-secret" {
		t.Fatalf("old ciphertext after rotation: %q %v", got, err)
	}
	newCT := mustEncrypt(t, "channel-secret", rotated)
	if id, ok := KeyIDOf(newCT); !ok || id != newPrimary {
		t.Errorf("new ciphertext uses key %d, want the new primary %d", id, newPrimary)
	}
	if id, ok := KeyIDOf(oldCT); !ok || id != oldPrimary {
		t.Errorf("old ciphertext is tagged with key %d, want %d", id, oldPrimary)
	}

	keys, _ := DescribeKeySet(rotated)
	if len(keys) != 2 {
		t.Fatalf("keys = %+v", keys)
	}
	for _, k := range keys {
		if !k.Enabled || !k.AESGCM || k.Primary != (k.ID == newPrimary) {
			t.Errorf("key %+v", k)
		}
	}
}

// 刪除舊金鑰：只留主要金鑰；還沒重新加密的舊密文從此解不開（所以要先重新加密）.
func TestPruneRemovesEverythingButThePrimary(t *testing.T) {
	ks := mustNewKeySet(t)
	oldCT := mustEncrypt(t, "token", ks)
	rotated, newPrimary, _ := RotateKeySet(ks)
	reCT := mustEncrypt(t, "token", rotated)

	pruned, removed, err := PruneKeySet(rotated)
	if err != nil || len(removed) != 1 {
		t.Fatalf("prune: removed %v err %v", removed, err)
	}
	keys, _ := DescribeKeySet(pruned)
	if len(keys) != 1 || keys[0].ID != newPrimary || !keys[0].Primary {
		t.Fatalf("keys after prune = %+v", keys)
	}
	if got, err := Decrypt(reCT, pruned); err != nil || got != "token" {
		t.Errorf("re-encrypted value: %q %v", got, err)
	}
	if _, err := Decrypt(oldCT, pruned); err == nil {
		t.Error("a value still under the removed key must no longer decrypt")
	}
	if _, removedAgain, _ := PruneKeySet(pruned); len(removedAgain) != 0 {
		t.Errorf("nothing left to prune: %v", removedAgain)
	}
}

func TestKeyIDOfRejectsForeignData(t *testing.T) {
	for _, s := range []string{
		"", "not base64!", base64.StdEncoding.EncodeToString([]byte{0x01, 0, 0}),
		base64.StdEncoding.EncodeToString([]byte{0x00, 1, 2, 3, 4, 5}),
	} {
		if _, ok := KeyIDOf(s); ok {
			t.Errorf("KeyIDOf(%q) should not recognize it", s)
		}
	}
}

// 只接受 AES-GCM 金鑰集（例如 ChaCha20 的金鑰集要被拒絕）.
func TestOnlyAESGCMKeySetsAreAccepted(t *testing.T) {
	handle, err := keyset.NewHandle(aead.ChaCha20Poly1305KeyTemplate())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := insecurecleartextkeyset.Write(handle, keyset.NewJSONWriter(&buf)); err != nil {
		t.Fatal(err)
	}
	chacha := base64.StdEncoding.EncodeToString(buf.Bytes())

	if err := ValidateKeySet(chacha); !errors.Is(err, errNotAESGCM) {
		t.Errorf("validate: %v", err)
	}
	if _, err := Encrypt("x", chacha); !errors.Is(err, errNotAESGCM) {
		t.Errorf("encrypt: %v", err)
	}
	if _, _, err := RotateKeySet(chacha); !errors.Is(err, errNotAESGCM) {
		t.Errorf("rotate: %v", err)
	}
	if err := ValidateKeySet(mustNewKeySet(t)); err != nil {
		t.Errorf("a fresh AES-GCM keyset is valid: %v", err)
	}
}
