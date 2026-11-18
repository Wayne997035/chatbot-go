package crypto

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
)

// 本檔所有 keyset 與 ENC 值都在執行期現產，不寫任何字面值，也不印任何機密.

const (
	fieldToken  = "line.channel_token"
	fieldSecret = "line.channel_secret"
)

func payloadBytes(t *testing.T, enc string) []byte {
	t.Helper()
	ct, _, err := parseENC(enc)
	if err != nil {
		t.Fatalf("parseENC: %v", err)
	}
	return ct
}

func v1FromBytes(ct []byte) string {
	return "ENC(v1:" + base64.StdEncoding.EncodeToString(ct) + ")"
}

func mustEncrypt(t *testing.T, plain, ks, path string) string {
	t.Helper()
	enc, err := EncryptValue(plain, ks, path)
	if err != nil {
		t.Fatalf("EncryptValue: %v", err)
	}
	return enc
}

func mustRotate(t *testing.T, ks string) string {
	t.Helper()
	out, err := RotateKeySet(ks)
	if err != nil {
		t.Fatalf("RotateKeySet: %v", err)
	}
	return out
}

func mustPrimary(t *testing.T, ks string) uint32 {
	t.Helper()
	id, err := PrimaryKeyID(ks)
	if err != nil {
		t.Fatalf("PrimaryKeyID: %v", err)
	}
	return id
}

// keysetWithExtraKey 在一把合法 AES-256-GCM 之外再加一把 add 指定的金鑰；primary 決定新鑰是否設為主金鑰.
func keysetWithExtraKey(t *testing.T, add func(*keyset.Manager) (uint32, error), primary bool) string {
	t.Helper()
	handle, err := loadKeyset(mustKeySet(t, 1))
	if err != nil {
		t.Fatalf("loadKeyset: %v", err)
	}
	manager := keyset.NewManagerFromHandle(handle)
	id, err := add(manager)
	if err != nil {
		t.Fatalf("add key: %v", err)
	}
	if primary {
		if err := manager.SetPrimary(id); err != nil {
			t.Fatalf("set primary: %v", err)
		}
	}
	out, err := manager.Handle()
	if err != nil {
		t.Fatalf("manager handle: %v", err)
	}
	ks, err := encodeKeyset(out)
	if err != nil {
		t.Fatalf("encodeKeyset: %v", err)
	}
	return ks
}

// surgery 直接改 keyset JSON（模擬被手動竄改的 keyset）.
func surgery(t *testing.T, ks string, edit func(doc map[string]any)) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(ks)
	if err != nil {
		t.Fatalf("decode keyset: %v", err)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal keyset: %v", err)
	}
	edit(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal keyset: %v", err)
	}
	return base64.StdEncoding.EncodeToString(out)
}

// [F34-33] AAD 位元組兩個方向都以字面值釘死.
func TestEncryptValue_RoundTrip_V1(t *testing.T) {
	ks := mustKeySet(t, 3)
	enc := mustEncrypt(t, "hello", ks, fieldToken)
	if !strings.HasPrefix(enc, "ENC(v1:") || !strings.HasSuffix(enc, ")") {
		t.Fatalf("format = %q..., want ENC(v1:...)", enc[:7])
	}
	plain, legacy, err := DecryptValue(enc, ks, fieldToken)
	if err != nil || plain != "hello" || legacy {
		t.Fatalf("DecryptValue = %q legacy=%v err=%v", plain, legacy, err)
	}

	primitive, err := getAEAD(ks)
	if err != nil {
		t.Fatalf("getAEAD: %v", err)
	}
	const literalAAD = "chatbot-go.config.v1:line.channel_token"
	// 方向一：EncryptValue 的輸出能用字面 AAD 解開.
	got, err := primitive.Decrypt(payloadBytes(t, enc), []byte(literalAAD))
	if err != nil || string(got) != "hello" {
		t.Fatalf("literal-AAD decrypt = %q err=%v", got, err)
	}
	// 負對照：nil AAD 解不開.
	if _, err := primitive.Decrypt(payloadBytes(t, enc), nil); err == nil {
		t.Fatal("nil AAD must not decrypt a v1 value")
	}
	// 方向二：以字面 AAD 加密的值能被 DecryptValue 解開.
	ct, err := primitive.Encrypt([]byte("world"), []byte(literalAAD))
	if err != nil {
		t.Fatalf("primitive encrypt: %v", err)
	}
	plain, legacy, err = DecryptValue(v1FromBytes(ct), ks, fieldToken)
	if err != nil || plain != "world" || legacy {
		t.Fatalf("DecryptValue(literal) = %q legacy=%v err=%v", plain, legacy, err)
	}
}

// [F34-33] 空路徑與空 keyset 都明確回錯.
func TestEncryptValue_RejectsEmptyPathAndKeyset(t *testing.T) {
	ks := mustKeySet(t, 1)
	if _, err := EncryptValue("x", ks, ""); err == nil {
		t.Error("empty field path must fail")
	}
	if _, err := EncryptValue("x", "", fieldToken); err == nil || !strings.Contains(err.Error(), "no keyset") {
		t.Errorf("empty keyset err = %v, want no keyset", err)
	}
	enc := mustEncrypt(t, "x", ks, fieldToken)
	if _, _, err := DecryptValue(enc, "", fieldToken); err == nil || !strings.Contains(err.Error(), "no keyset") {
		t.Errorf("decrypt with empty keyset err = %v, want no keyset", err)
	}
}

func TestDecryptValue_WrongKeyset_Fails(t *testing.T) {
	ks1, ks2 := mustKeySet(t, 2), mustKeySet(t, 2)
	enc := mustEncrypt(t, "secret", ks1, fieldToken)
	plain, _, err := DecryptValue(enc, ks2, fieldToken)
	if err == nil || plain != "" {
		t.Fatalf("wrong keyset: plain=%q err=%v, want error and empty plain", plain, err)
	}
	// 正對照：同一把 keyset 解得開，證明上面的失敗來自 keyset.
	if got, _, err := DecryptValue(enc, ks1, fieldToken); err != nil || got != "secret" {
		t.Fatalf("control decrypt = %q err=%v", got, err)
	}
}

func TestDecryptValue_FieldSwap_AADMismatch(t *testing.T) {
	ks := mustKeySet(t, 2)
	enc := mustEncrypt(t, "secret", ks, fieldToken)
	plain, _, err := DecryptValue(enc, ks, fieldSecret)
	if err == nil || plain != "" || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("field swap: plain=%q err=%v, want authentication failed", plain, err)
	}
	if got, _, err := DecryptValue(enc, ks, fieldToken); err != nil || got != "secret" {
		t.Fatalf("control decrypt = %q err=%v", got, err)
	}
}

func TestDecryptValue_TamperOneByte(t *testing.T) {
	ks := mustKeySet(t, 2)
	ct := payloadBytes(t, mustEncrypt(t, "secret", ks, fieldToken))
	for name, idx := range map[string]int{"first": 0, "middle": len(ct) / 2, "last": len(ct) - 1} {
		t.Run(name, func(t *testing.T) {
			tampered := bytes.Clone(ct)
			tampered[idx] ^= 0x01
			plain, _, err := DecryptValue(v1FromBytes(tampered), ks, fieldToken)
			if err == nil || plain != "" || !strings.Contains(err.Error(), "authentication failed") {
				t.Fatalf("tamper %s: plain=%q err=%v, want authentication failed", name, plain, err)
			}
		})
	}
}

func TestDecryptValue_Truncated(t *testing.T) {
	ks := mustKeySet(t, 2)
	ct := payloadBytes(t, mustEncrypt(t, "secret", ks, fieldToken))
	for name, n := range map[string]int{"len-1": len(ct) - 1, "4": 4, "0": 0} {
		t.Run(name, func(t *testing.T) {
			plain, _, err := DecryptValue(v1FromBytes(ct[:n]), ks, fieldToken)
			if err == nil || plain != "" {
				t.Fatalf("truncate to %s: plain=%q err=%v, want error", name, plain, err)
			}
		})
	}
}

func TestDecryptValue_MalformedFormat(t *testing.T) {
	ks := mustKeySet(t, 1)
	tests := []struct {
		in      string
		wantErr string
	}{
		{"ENC()", "bad format"},
		{"ENC(v1:)", "bad format"},
		{"ENC(v1:!!!)", "bad format"},
		// 若被當 v1 解，base64 後只有 3 byte 也會回錯；只斷言「有錯」打不死「未知版本當 v1」，所以要斷言類別.
		{"ENC(v9:AAAA)", "unsupported version"},
		{"ENC(v1)", "unsupported version"},
		{"ENC(v1:AAAA", "bad format"},
		{"plain-value", "bad format"},
		{"", "bad format"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			plain, _, err := DecryptValue(tc.in, ks, fieldToken)
			if err == nil || plain != "" || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("DecryptValue(%q) plain=%q err=%v, want %q", tc.in, plain, err, tc.wantErr)
			}
		})
	}
}

// [F34-33] 舊格式 ENC(<b64>)：nil AAD、legacy=true.
func TestDecryptValue_LegacyFormat(t *testing.T) {
	ks := mustKeySet(t, 2)
	ct, err := Encrypt("legacy-value", ks)
	if err != nil {
		t.Fatalf("legacy Encrypt: %v", err)
	}
	plain, legacy, err := DecryptValue("ENC("+ct+")", ks, fieldToken)
	if err != nil || plain != "legacy-value" || !legacy {
		t.Fatalf("legacy decrypt = %q legacy=%v err=%v", plain, legacy, err)
	}
	// 負對照：v1 格式不是 legacy.
	_, legacy, err = DecryptValue(mustEncrypt(t, "x", ks, fieldToken), ks, fieldToken)
	if err != nil || legacy {
		t.Fatalf("v1 value: legacy=%v err=%v, want false/nil", legacy, err)
	}
}

func TestDecryptValue_ErrorDoesNotLeak(t *testing.T) {
	const sentinel = "SENTINEL-PLAIN-7f3a"
	ks1, ks2 := mustKeySet(t, 2), mustKeySet(t, 2)
	enc := mustEncrypt(t, sentinel, ks1, fieldToken)
	payload := strings.TrimSuffix(strings.TrimPrefix(enc, "ENC(v1:"), ")")

	_, _, wrongKey := DecryptValue(enc, ks2, fieldToken)
	_, _, wrongPath := DecryptValue(enc, ks1, fieldSecret)
	_, _, badFmt := DecryptValue("ENC(v1:"+sentinel+"!)", ks1, fieldToken)
	_, _, badKeyset := DecryptValue(enc, "KMS("+sentinel+")", fieldToken)
	for name, err := range map[string]error{"wrongKey": wrongKey, "wrongPath": wrongPath, "badFmt": badFmt, "badKeyset": badKeyset} {
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		for _, secret := range []string{sentinel, payload, ks1, ks2, enc} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error text leaks input", name)
			}
		}
	}
}

// 以下五個函式對應規格的 TestValidateKeySet_Rejects* 一列：[F34-34] 每種不合格金鑰逐列回錯.
func TestValidateKeySet_RejectsAES128(t *testing.T) {
	for _, primary := range []bool{true, false} {
		ks := keysetWithExtraKey(t, func(m *keyset.Manager) (uint32, error) { return m.Add(aead.AES128GCMKeyTemplate()) }, primary)
		if err := ValidateKeySet(ks); err == nil {
			t.Errorf("AES128 (primary=%v) must be rejected", primary)
		}
	}
	if err := ValidateKeySet(mustKeySet(t, 1)); err != nil {
		t.Fatalf("control AES256 keyset rejected: %v", err)
	}
}

func TestValidateKeySet_RejectsXChaCha20(t *testing.T) {
	for _, primary := range []bool{true, false} {
		ks := keysetWithExtraKey(t, func(m *keyset.Manager) (uint32, error) { return m.Add(aead.XChaCha20Poly1305KeyTemplate()) }, primary)
		if err := ValidateKeySet(ks); err == nil {
			t.Errorf("XChaCha20 (primary=%v) must be rejected", primary)
		}
	}
}

func TestValidateKeySet_RejectsSIV(t *testing.T) {
	for _, primary := range []bool{true, false} {
		ks := keysetWithExtraKey(t, func(m *keyset.Manager) (uint32, error) { return m.Add(aead.AES256GCMSIVKeyTemplate()) }, primary)
		if err := ValidateKeySet(ks); err == nil {
			t.Errorf("AES256-GCM-SIV (primary=%v) must be rejected", primary)
		}
	}
}

// NoPrefix 變體型別與長度都過，只有變體檢查擋得住；驗證必須在快取之前，所以連續兩次、且 EncryptValue 都要回錯.
func TestValidateKeySet_RejectsNoPrefix(t *testing.T) {
	ks := keysetWithExtraKey(t, func(m *keyset.Manager) (uint32, error) { return m.Add(aead.AES256GCMNoPrefixKeyTemplate()) }, true)
	for i := range 2 {
		if err := ValidateKeySet(ks); err == nil {
			t.Fatalf("ValidateKeySet call %d accepted a NoPrefix keyset", i+1)
		}
	}
	if enc, err := EncryptValue("x", ks, fieldToken); err == nil {
		t.Fatalf("EncryptValue accepted a NoPrefix keyset (enc len %d)", len(enc))
	}
}

func TestValidateKeySet_RejectsDisabledNonPrimary(t *testing.T) {
	ks := mustKeySet(t, 2)
	handle, err := loadKeyset(ks)
	if err != nil {
		t.Fatal(err)
	}
	infos, err := KeyIDs(ks)
	if err != nil {
		t.Fatal(err)
	}
	var nonPrimary uint32
	for _, info := range infos {
		if !info.Primary {
			nonPrimary = info.ID
		}
	}
	manager := keyset.NewManagerFromHandle(handle)
	if err := manager.Disable(nonPrimary); err != nil {
		t.Fatalf("disable: %v", err)
	}
	disabled, err := manager.Handle()
	if err != nil {
		t.Fatal(err)
	}
	disabledKS, err := encodeKeyset(disabled)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateKeySet(disabledKS); err == nil {
		t.Fatal("keyset with a DISABLED non-primary key must be rejected")
	}
}

// 主金鑰 DISABLED、primaryKeyId 指向不存在的鍵：由 Tink 的 keyset.Read 那層擋，不是本單的 ENABLED 檢查擋的.
func TestValidateKeySet_RejectsBadPrimary(t *testing.T) {
	ks := mustKeySet(t, 2)
	primary := mustPrimary(t, ks)

	disablePrimary := surgery(t, ks, func(doc map[string]any) {
		for _, k := range doc["key"].([]any) {
			entry := k.(map[string]any)
			if uint32(entry["keyId"].(float64)) == primary {
				entry["status"] = "DISABLED"
			}
		}
	})
	infos, err := KeyIDs(ks)
	if err != nil {
		t.Fatal(err)
	}
	missing := primary + 1
	for taken := true; taken; {
		taken = false
		for _, info := range infos {
			if info.ID == missing {
				taken = true
				missing++
			}
		}
	}
	missingPrimary := surgery(t, ks, func(doc map[string]any) {
		doc["primaryKeyId"] = float64(missing)
	})
	for name, bad := range map[string]string{"primary DISABLED": disablePrimary, "primaryKeyId missing": missingPrimary} {
		if err := ValidateKeySet(bad); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}

// [F34-34] GenerateKeySet(5)：5 把皆過驗證、恰一把 primary；前綴版面以實際輸出驗證.
func TestGenerateKeySet_FiveAES256Keys(t *testing.T) {
	ks := mustKeySet(t, 5)
	if err := ValidateKeySet(ks); err != nil {
		t.Fatalf("ValidateKeySet: %v", err)
	}
	infos, err := KeyIDs(ks)
	if err != nil || len(infos) != 5 {
		t.Fatalf("KeyIDs len=%d err=%v, want 5", len(infos), err)
	}
	seen, primaries := map[uint32]bool{}, 0
	for _, info := range infos {
		seen[info.ID] = true
		if info.Primary {
			primaries++
		}
		if info.Status != keyset.Enabled.String() {
			t.Errorf("key %d status = %s, want ENABLED", info.ID, info.Status)
		}
	}
	if len(seen) != 5 || primaries != 1 {
		t.Fatalf("distinct ids=%d primaries=%d, want 5 and 1", len(seen), primaries)
	}
	ct := payloadBytes(t, mustEncrypt(t, "x", ks, fieldToken))
	if ct[0] != 0x01 || binary.BigEndian.Uint32(ct[1:5]) != mustPrimary(t, ks) {
		t.Fatalf("prefix layout wrong: byte0=%#x id=%d primary=%d", ct[0], binary.BigEndian.Uint32(ct[1:5]), mustPrimary(t, ks))
	}
}

func TestRotateKeySet_KeepsOldAddsPrimary(t *testing.T) {
	k1 := mustKeySet(t, 2)
	oldPrimary := mustPrimary(t, k1)
	oldEnc := mustEncrypt(t, "old-value", k1, fieldToken)

	k2 := mustRotate(t, k1)
	if err := ValidateKeySet(k2); err != nil {
		t.Fatalf("rotated keyset invalid: %v", err)
	}
	infos, err := KeyIDs(k2)
	if err != nil || len(infos) != 3 {
		t.Fatalf("rotated key count = %d err=%v, want 3", len(infos), err)
	}
	newPrimary := mustPrimary(t, k2)
	if newPrimary == oldPrimary {
		t.Fatal("primary id did not change after rotation")
	}
	if plain, _, err := DecryptValue(oldEnc, k2, fieldToken); err != nil || plain != "old-value" {
		t.Fatalf("old value after rotation = %q err=%v", plain, err)
	}
	ct := payloadBytes(t, mustEncrypt(t, "new-value", k2, fieldToken))
	if binary.BigEndian.Uint32(ct[1:5]) != newPrimary {
		t.Fatal("new value is not encrypted under the new primary")
	}
	if _, err := RotateKeySet("not-a-keyset"); err == nil {
		t.Fatal("RotateKeySet must reject an invalid keyset")
	}
}

func TestNeedsReencrypt(t *testing.T) {
	k1 := mustKeySet(t, 2)
	k2 := mustRotate(t, k1)
	legacyCT, err := Encrypt("v", k2)
	if err != nil {
		t.Fatal(err)
	}
	short := v1FromBytes([]byte{0x01, 0x00, 0x00})
	wrongByte0 := v1FromBytes([]byte{0x02, 0, 0, 0, 1, 9, 9})

	tests := []struct {
		name    string
		enc     string
		want    bool
		wantErr bool
	}{
		{"legacy format", "ENC(" + legacyCT + ")", true, false},
		{"old primary", mustEncrypt(t, "v", k1, fieldToken), true, false},
		{"current primary", mustEncrypt(t, "v", k2, fieldToken), false, false},
		{"too short", short, false, true},
		{"byte0 not 0x01", wrongByte0, false, true},
		{"malformed", "ENC(v9:AAAA)", false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NeedsReencrypt(tc.enc, k2)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("NeedsReencrypt = %v err=%v, want %v wantErr=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
	if _, err := NeedsReencrypt(mustEncrypt(t, "v", k2, fieldToken), ""); err == nil {
		t.Error("NeedsReencrypt with empty keyset must fail")
	}
}

// [F34-35] 前綴 byte1-4 大端 key id.
func TestCiphertextKeyID(t *testing.T) {
	ks := mustKeySet(t, 3)
	primary := mustPrimary(t, ks)
	legacyCT, err := Encrypt("v", ks)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		enc     string
		want    uint32
		wantErr bool
	}{
		{"v1 value", mustEncrypt(t, "v", ks, fieldToken), primary, false},
		{"legacy value", "ENC(" + legacyCT + ")", primary, false},
		{"shorter than 5 bytes", v1FromBytes([]byte{0x01, 0, 0, 1}), 0, true},
		{"byte0 not 0x01", v1FromBytes([]byte{0x00, 0, 0, 0, 1, 7}), 0, true},
		{"not ENC", "nope", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CiphertextKeyID(tc.enc)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("CiphertextKeyID = %d err=%v, want %d wantErr=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// [F34-35] 移除用 Manager.Delete；primary 與不存在的 id 回錯且回傳空字串.
func TestPruneKeySet(t *testing.T) {
	k1 := mustKeySet(t, 2)
	oldPrimary := mustPrimary(t, k1)
	oldEnc := mustEncrypt(t, "old", k1, fieldToken)
	k2 := mustRotate(t, k1)
	newPrimary := mustPrimary(t, k2)
	newEnc := mustEncrypt(t, "new", k2, fieldToken)

	pruned, err := PruneKeySet(k2, []uint32{oldPrimary})
	if err != nil {
		t.Fatalf("PruneKeySet: %v", err)
	}
	if err := ValidateKeySet(pruned); err != nil {
		t.Fatalf("pruned keyset invalid: %v", err)
	}
	infos, err := KeyIDs(pruned)
	if err != nil || len(infos) != 2 {
		t.Fatalf("pruned key count = %d err=%v, want 2", len(infos), err)
	}
	for _, info := range infos {
		if info.ID == oldPrimary {
			t.Fatal("removed key id still present")
		}
	}
	if got := mustPrimary(t, pruned); got != newPrimary {
		t.Fatalf("primary changed: %d -> %d", newPrimary, got)
	}
	if plain, _, err := DecryptValue(newEnc, pruned, fieldToken); err != nil || plain != "new" {
		t.Fatalf("new value after prune = %q err=%v", plain, err)
	}
	if _, _, err := DecryptValue(oldEnc, pruned, fieldToken); err == nil {
		t.Fatal("value under the removed key must no longer decrypt")
	}

	for name, remove := range map[string][]uint32{"primary": {newPrimary}, "missing id": {newPrimary + 1}} {
		out, err := PruneKeySet(k2, remove)
		if err == nil || out != "" {
			t.Errorf("%s: out-len=%d err=%v, want error and empty string", name, len(out), err)
		}
	}
}

// [F34-36] 接縫：keyset 外面包一層（假 KMS）再解開，舊 ENC 值免重加密.
func TestKeysetWrap_OldENCStillDecrypts(t *testing.T) {
	paths := []string{"database.mongo.uri", fieldToken, "security.admin_token"}
	plains := []string{"value-one", "value-two", "value-three"}
	k := mustKeySet(t, 5)
	encs := make([]string, len(paths))
	idsBefore := make([]uint32, len(paths))
	for i := range paths {
		encs[i] = mustEncrypt(t, plains[i], k, paths[i])
		id, err := CiphertextKeyID(encs[i])
		if err != nil {
			t.Fatal(err)
		}
		idsBefore[i] = id
	}
	primaryBefore := mustPrimary(t, k)

	// 假 KMS：測試內現產的另一組本地 keyset.
	master, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	if err != nil {
		t.Fatal(err)
	}
	masterAEAD, err := aead.New(master)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := loadKeyset(k)
	if err != nil {
		t.Fatal(err)
	}
	var wrapped bytes.Buffer
	if err := handle.Write(keyset.NewJSONWriter(&wrapped), masterAEAD); err != nil {
		t.Fatalf("wrap keyset: %v", err)
	}
	wrappedBytes := wrapped.Bytes()

	// 負對照：另一把假 KMS 解不開.
	otherMaster, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	if err != nil {
		t.Fatal(err)
	}
	otherAEAD, err := aead.New(otherMaster)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyset.Read(keyset.NewJSONReader(bytes.NewReader(wrappedBytes)), otherAEAD); err == nil {
		t.Fatal("unwrapping with a different master key must fail")
	}

	unwrapped, err := keyset.Read(keyset.NewJSONReader(bytes.NewReader(wrappedBytes)), masterAEAD)
	if err != nil {
		t.Fatalf("unwrap keyset: %v", err)
	}
	var plainKS bytes.Buffer
	if err := insecurecleartextkeyset.Write(unwrapped, keyset.NewJSONWriter(&plainKS)); err != nil {
		t.Fatal(err)
	}
	k2 := base64.StdEncoding.EncodeToString(plainKS.Bytes())

	if got := mustPrimary(t, k2); got != primaryBefore {
		t.Fatalf("primary id changed by wrapping: %d -> %d", primaryBefore, got)
	}
	for i := range paths {
		plain, legacy, err := DecryptValue(encs[i], k2, paths[i])
		if err != nil || plain != plains[i] || legacy {
			t.Fatalf("%s after unwrap = %q legacy=%v err=%v", paths[i], plain, legacy, err)
		}
		if id, err := CiphertextKeyID(encs[i]); err != nil || id != idsBefore[i] {
			t.Fatalf("%s key id changed: %d -> %d err=%v", paths[i], idsBefore[i], id, err)
		}
	}
}

// [F34-36] KMS( 開頭一律 fail-closed：不回 handle、不寫快取、錯誤不帶括號內容.
func TestLoadKeyset_KMSPrefixFailsClosed(t *testing.T) {
	const inner = "SENTINEL-KS-4c1e"
	for _, raw := range []string{"KMS(" + inner + ")", "  KMS(" + inner + ")  ", "KMS(anything)"} {
		handle, err := loadKeyset(raw)
		if handle != nil || err == nil || !strings.Contains(err.Error(), "unsupported keyset source") || strings.Contains(err.Error(), inner) {
			t.Errorf("loadKeyset(%q) handle-nil=%v err=%v", raw, handle == nil, err)
		}
		vErr := ValidateKeySet(raw)
		if vErr == nil || !strings.Contains(vErr.Error(), "unsupported keyset source") || strings.Contains(vErr.Error(), inner) {
			t.Errorf("ValidateKeySet(%q) err=%v", raw, vErr)
		}
		for _, key := range []string{raw, strings.TrimSpace(raw)} {
			if _, ok := aeadCache.Load(key); ok {
				t.Errorf("aeadCache holds an entry for %q", key)
			}
		}
	}
}

// [F34-36] 所有讀 keyset 的路徑都經 loadKeyset：掃描非測試原始碼，insecurecleartextkeyset.Read 只能出現在 loadKeyset 本體.
func TestLoadKeyset_IsTheOnlyKeysetReader(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	fset := token.NewFileSet()
	var readers []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Read" {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "insecurecleartextkeyset" {
					readers = append(readers, fn.Name.Name)
				}
				return true
			})
		}
	}
	if len(readers) != 1 || readers[0] != "loadKeyset" {
		t.Fatalf("insecurecleartextkeyset.Read callers = %v, want exactly [loadKeyset]", readers)
	}
}
