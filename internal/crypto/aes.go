package crypto

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/aead/aesgcm"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
)

type aeadPrimitive interface {
	Encrypt(pt, aad []byte) ([]byte, error)
	Decrypt(ct, aad []byte) ([]byte, error)
}

// aesKeyBytes 是唯一接受的金鑰長度（AES-256）.
const aesKeyBytes = 32

// kmsKeysetPrefix 預留給日後「KMS 包起來的 keyset」；現在一律 fail-closed.
const kmsKeysetPrefix = "KMS("

// 錯誤只分這幾類，訊息不帶輸入、密文、明文、keyset，也不 %w Tink 的錯誤.
var (
	errNoKeyset                = errors.New("no keyset")
	errUnsupportedKeysetSource = errors.New("unsupported keyset source")
	errInvalidKeyset           = errors.New("invalid keyset")
)

var aeadCache sync.Map

// loadKeyset 是整個套件讀取 keyset 的唯一入口（TestLoadKeyset_IsTheOnlyKeysetReader 掃描把關）.
// [F34-36] 日後換 KMS 只需在這裡多一個來源；`KMS(` 開頭現在 fail-closed，不退回空 keyset、不當明文解析.
func loadKeyset(raw string) (*keyset.Handle, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errNoKeyset
	}
	if strings.HasPrefix(raw, kmsKeysetPrefix) {
		return nil, errUnsupportedKeysetSource
	}

	keysetJSON, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: not base64", errInvalidKeyset)
	}

	handle, err := insecurecleartextkeyset.Read(keyset.NewJSONReader(bytes.NewReader(keysetJSON)))
	if err != nil {
		return nil, fmt.Errorf("%w: unreadable", errInvalidKeyset)
	}
	return handle, nil
}

// loadValidated 讀入並驗證 keyset；所有需要「可信 keyset」的函式都走這裡.
func loadValidated(raw string) (*keyset.Handle, error) {
	handle, err := loadKeyset(raw)
	if err != nil {
		return nil, err
	}
	if err := validateHandle(handle); err != nil {
		return nil, err
	}
	return handle, nil
}

// validateHandle [F34-34] 每把金鑰必須是 AES-256-GCM、Tink 前綴變體、ENABLED，且有 primary.
// 要求 VariantTink：NoPrefix 變體型別與長度都過，但密文沒有 key id 前綴，NeedsReencrypt 會讀錯.
// 主金鑰 DISABLED 或 primaryKeyId 不存在由 Tink 的 keyset.Read 擋；這裡擋的是非主金鑰的狀態.
func validateHandle(h *keyset.Handle) error {
	hasPrimary := false
	for i := range h.Len() {
		entry, err := h.Entry(i)
		if err != nil {
			return fmt.Errorf("%w: key %d unreadable", errInvalidKeyset, i+1)
		}
		key, ok := entry.Key().(*aesgcm.Key)
		if !ok {
			return fmt.Errorf("%w: key %d is not AES-256-GCM", errInvalidKeyset, i+1)
		}
		if err := checkAESParams(key, i+1); err != nil {
			return err
		}
		if entry.KeyStatus() != keyset.Enabled {
			return fmt.Errorf("%w: key %d is not enabled", errInvalidKeyset, i+1)
		}
		if entry.IsPrimary() {
			hasPrimary = true
		}
	}
	if !hasPrimary {
		return fmt.Errorf("%w: no primary key", errInvalidKeyset)
	}
	return nil
}

func checkAESParams(key *aesgcm.Key, n int) error {
	params, ok := key.Parameters().(*aesgcm.Parameters)
	if !ok {
		return fmt.Errorf("%w: key %d is not AES-256-GCM", errInvalidKeyset, n)
	}
	if params.KeySizeInBytes() != aesKeyBytes {
		return fmt.Errorf("%w: key %d is not 32 bytes", errInvalidKeyset, n)
	}
	if params.Variant() != aesgcm.VariantTink {
		return fmt.Errorf("%w: key %d lacks the Tink prefix variant", errInvalidKeyset, n)
	}
	return nil
}

// getAEAD 取得（快取的）AEAD primitive；驗證一定在寫入快取之前.
func getAEAD(keysetBase64 string) (aeadPrimitive, error) {
	keysetBase64 = strings.TrimSpace(keysetBase64)

	if cached, ok := aeadCache.Load(keysetBase64); ok {
		return cached.(aeadPrimitive), nil
	}

	handle, err := loadValidated(keysetBase64)
	if err != nil {
		return nil, err
	}

	primitive, err := aead.New(handle)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot build primitive", errInvalidKeyset)
	}

	aeadCache.Store(keysetBase64, primitive)
	return primitive, nil
}

// ValidateKeySet 驗證 keyset 是否可被載入為 Tink AEAD（含型別、長度、變體、狀態檢查）.
func ValidateKeySet(keysetBase64 string) error {
	_, err := getAEAD(keysetBase64)
	return err
}

// Encrypt 使用 Tink AEAD 加密，回傳 base64 ciphertext.
//
// Deprecated: nil AAD, legacy only. 新程式碼用 EncryptValue（AAD 綁欄位路徑）.
func Encrypt(plaintext, keysetBase64 string) (string, error) {
	primitive, err := getAEAD(keysetBase64)
	if err != nil {
		return "", err
	}

	ciphertext, err := primitive.Encrypt([]byte(plaintext), nil)
	if err != nil {
		return "", fmt.Errorf("encrypt: %w", err)
	}

	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt 使用 Tink AEAD 解密 base64 ciphertext.
//
// Deprecated: nil AAD, legacy only. 新程式碼用 DecryptValue.
func Decrypt(encrypted, keysetBase64 string) (string, error) {
	primitive, err := getAEAD(keysetBase64)
	if err != nil {
		return "", err
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}

	plaintext, err := primitive.Decrypt(ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}

	return string(plaintext), nil
}

// GenerateKeySet 產生包含 n 把 AES256-GCM key 的 Tink JSON keyset（base64 編碼）.
func GenerateKeySet(n int) (string, error) {
	if n < 1 {
		return "", fmt.Errorf("key count must be >= 1")
	}

	handle, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	if err != nil {
		return "", fmt.Errorf("create keyset: %w", err)
	}

	manager := keyset.NewManagerFromHandle(handle)
	for i := 1; i < n; i++ {
		keyID, err := manager.Add(aead.AES256GCMKeyTemplate())
		if err != nil {
			return "", fmt.Errorf("add key to keyset: %w", err)
		}
		if err := manager.SetPrimary(keyID); err != nil {
			return "", fmt.Errorf("set primary key: %w", err)
		}
	}
	handle, err = manager.Handle()
	if err != nil {
		return "", fmt.Errorf("finalize keyset: %w", err)
	}

	return encodeKeyset(handle)
}

// encodeKeyset 把 handle 序列化成 base64 JSON keyset（明文來源格式）.
func encodeKeyset(handle *keyset.Handle) (string, error) {
	var buf bytes.Buffer
	if err := insecurecleartextkeyset.Write(handle, keyset.NewJSONWriter(&buf)); err != nil {
		return "", fmt.Errorf("write keyset: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

const (
	encPrefix  = "ENC("
	encSuffix  = ")"
	v1Marker   = "v1:"
	aadDomain  = "chatbot-go.config.v1:"
	tinkPrefix = 0x01 // Tink VariantTink 密文第 0 byte；其後 4 byte 是大端 key id.
	tinkHeader = 5
)

// 解密端的錯誤只分這四類；訊息固定，不帶任何輸入.
var (
	errBadFormat          = errors.New("bad format")
	errUnsupportedVersion = errors.New("unsupported version")
	errAuthFailed         = errors.New("authentication failed")
)

// KeyInfo 描述 keyset 內一把金鑰（不含金鑰內容）.
type KeyInfo struct {
	ID      uint32
	Primary bool
	Status  string
}

// IsENC 回傳 s 是否為 ENC(...) 形式的值.
func IsENC(s string) bool {
	return strings.HasPrefix(s, encPrefix) && strings.HasSuffix(s, encSuffix)
}

// fieldAAD [F34-33] AAD = 網域前綴 + yaml 路徑；把 A 欄位的密文貼到 B 欄位會解不開.
func fieldAAD(fieldPath string) []byte {
	return []byte(aadDomain + fieldPath)
}

// EncryptValue 以 keyset 的主金鑰加密，回傳 ENC(v1:<b64>)；AAD 綁定 fieldPath.
func EncryptValue(plaintext, keysetBase64, fieldPath string) (string, error) {
	if fieldPath == "" {
		return "", fmt.Errorf("%w: empty field path", errBadFormat)
	}
	primitive, err := getAEAD(keysetBase64)
	if err != nil {
		return "", err
	}
	ciphertext, err := primitive.Encrypt([]byte(plaintext), fieldAAD(fieldPath))
	if err != nil {
		return "", errors.New("encrypt failed")
	}
	return encPrefix + v1Marker + base64.StdEncoding.EncodeToString(ciphertext) + encSuffix, nil
}

// parseENC 拆出密文位元組；legacy=true 代表舊格式 ENC(<b64>)（nil AAD）.
// 內容以 v1: 開頭 → 新格式；以 v 開頭但不是 v1: → 未知版本；其他 → 舊格式（Tink 前綴 0x01 的 base64 一律以 A 開頭）.
func parseENC(enc string) (ciphertext []byte, legacy bool, err error) {
	if !IsENC(enc) {
		return nil, false, errBadFormat
	}
	content := strings.TrimSpace(enc[len(encPrefix) : len(enc)-len(encSuffix)])
	payload := content
	switch {
	case strings.HasPrefix(content, v1Marker):
		payload = content[len(v1Marker):]
	case strings.HasPrefix(content, "v"):
		return nil, false, errUnsupportedVersion
	default:
		legacy = true
	}
	if payload == "" {
		return nil, false, errBadFormat
	}
	ciphertext, decodeErr := base64.StdEncoding.DecodeString(payload)
	if decodeErr != nil || len(ciphertext) == 0 {
		return nil, false, errBadFormat
	}
	return ciphertext, legacy, nil
}

// DecryptValue 解 ENC(...) 值；新格式帶 fieldPath AAD，舊格式用 nil AAD 並回 legacy=true.
func DecryptValue(enc, keysetBase64, fieldPath string) (plain string, legacy bool, err error) {
	ciphertext, legacy, err := parseENC(enc)
	if err != nil {
		return "", false, err
	}
	primitive, err := getAEAD(keysetBase64)
	if err != nil {
		return "", false, err
	}
	var aad []byte
	if !legacy {
		aad = fieldAAD(fieldPath)
	}
	plaintext, decErr := primitive.Decrypt(ciphertext, aad)
	if decErr != nil {
		return "", false, errAuthFailed
	}
	return string(plaintext), legacy, nil
}

// prefixKeyID 取 Tink 前綴 byte1-4（大端）；長度不足或 byte0 不是 0x01 回錯.
func prefixKeyID(ciphertext []byte) (uint32, error) {
	if len(ciphertext) < tinkHeader || ciphertext[0] != tinkPrefix {
		return 0, fmt.Errorf("%w: no key id prefix", errBadFormat)
	}
	return binary.BigEndian.Uint32(ciphertext[1:tinkHeader]), nil
}

// CiphertextKeyID [F34-35] 回傳加密當時的金鑰 id（舊格式與 v1 皆可）.
func CiphertextKeyID(enc string) (uint32, error) {
	ciphertext, _, err := parseENC(enc)
	if err != nil {
		return 0, err
	}
	return prefixKeyID(ciphertext)
}

// PrimaryKeyID [F34-35] 回傳 keyset 目前主金鑰的 id.
func PrimaryKeyID(keysetBase64 string) (uint32, error) {
	handle, err := loadValidated(keysetBase64)
	if err != nil {
		return 0, err
	}
	entry, err := handle.Primary()
	if err != nil {
		return 0, fmt.Errorf("%w: no primary key", errInvalidKeyset)
	}
	return entry.KeyID(), nil
}

// KeyIDs [F34-35] 列出每把金鑰的 id、是否 primary、狀態（不含金鑰內容）.
func KeyIDs(keysetBase64 string) ([]KeyInfo, error) {
	handle, err := loadValidated(keysetBase64)
	if err != nil {
		return nil, err
	}
	infos := make([]KeyInfo, 0, handle.Len())
	for i := range handle.Len() {
		entry, err := handle.Entry(i)
		if err != nil {
			return nil, fmt.Errorf("%w: key %d unreadable", errInvalidKeyset, i+1)
		}
		infos = append(infos, KeyInfo{ID: entry.KeyID(), Primary: entry.IsPrimary(), Status: entry.KeyStatus().String()})
	}
	return infos, nil
}

// RotateKeySet [F34-34] 追加一把新金鑰並設為主金鑰，舊金鑰全留（舊密文照樣可解）.
func RotateKeySet(keysetBase64 string) (string, error) {
	handle, err := loadValidated(keysetBase64)
	if err != nil {
		return "", err
	}
	manager := keyset.NewManagerFromHandle(handle)
	keyID, err := manager.Add(aead.AES256GCMKeyTemplate())
	if err != nil {
		return "", errors.New("rotate keyset: add key failed")
	}
	if err := manager.SetPrimary(keyID); err != nil {
		return "", errors.New("rotate keyset: set primary failed")
	}
	rotated, err := manager.Handle()
	if err != nil {
		return "", errors.New("rotate keyset: finalize failed")
	}
	return encodeKeyset(rotated)
}

// NeedsReencrypt [F34-34] 舊格式 → true；密文前綴的 key id 不是主金鑰 → true；已在主金鑰 → false.
func NeedsReencrypt(enc, keysetBase64 string) (bool, error) {
	primary, err := PrimaryKeyID(keysetBase64)
	if err != nil {
		return false, err
	}
	ciphertext, legacy, err := parseENC(enc)
	if err != nil {
		return false, err
	}
	if legacy {
		return true, nil
	}
	id, err := prefixKeyID(ciphertext)
	if err != nil {
		return false, err
	}
	return id != primary, nil
}

// PruneKeySet [F34-35] 以 Manager.Delete 把指定 id 的金鑰整把移除.
// 不用 Disable：停用的非主金鑰會被 validateHandle 擋下. 移除 primary 或不存在的 id 回錯（回傳空字串）.
func PruneKeySet(keysetBase64 string, remove []uint32) (string, error) {
	handle, err := loadValidated(keysetBase64)
	if err != nil {
		return "", err
	}
	manager := keyset.NewManagerFromHandle(handle)
	for _, id := range remove {
		if err := manager.Delete(id); err != nil {
			return "", fmt.Errorf("prune keyset: cannot remove key %d (missing or primary)", id)
		}
	}
	pruned, err := manager.Handle()
	if err != nil {
		return "", errors.New("prune keyset: finalize failed")
	}
	return encodeKeyset(pruned)
}
