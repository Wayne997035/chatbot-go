package crypto

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
	tinkpb "github.com/tink-crypto/tink-go/v2/proto/tink_go_proto"
)

// 金鑰輪替：金鑰集（keyset）裡可以有多把 AES-256-GCM 金鑰，其中一把是「主要」金鑰.
// 加密一律用主要金鑰；每個密文開頭記著加密時用的金鑰 ID，解密時自動找對應的金鑰，
// 所以加入新金鑰並設為主要之後，舊的密文照樣解得開；全部重新加密後再刪除舊金鑰.

// aesGCMKeyType 允許的金鑰類型：只接受 AES-GCM.
const aesGCMKeyType = "type.googleapis.com/google.crypto.tink.AesGcmKey"

// KeyInfo 金鑰集裡一把金鑰的資訊（不含金鑰本身）.
type KeyInfo struct {
	ID      uint32
	Primary bool
	Enabled bool
	AESGCM  bool
}

// NewKeySet 產生只有一把 AES-256-GCM 金鑰的新金鑰集（base64 編碼的 Tink JSON keyset）.
func NewKeySet() (string, error) {
	return GenerateKeySet(1)
}

// RotateKeySet 加入一把新的 AES-256-GCM 金鑰並設為主要金鑰；舊金鑰保留，舊密文仍可解密.
func RotateKeySet(keysetBase64 string) (rotated string, newPrimary uint32, err error) {
	handle, err := readHandle(keysetBase64)
	if err != nil {
		return "", 0, err
	}
	manager := keyset.NewManagerFromHandle(handle)
	newPrimary, err = manager.Add(aead.AES256GCMKeyTemplate())
	if err != nil {
		return "", 0, fmt.Errorf("add key: %w", err)
	}
	if err := manager.SetPrimary(newPrimary); err != nil {
		return "", 0, fmt.Errorf("set primary key: %w", err)
	}
	rotated, err = writeManager(manager)
	return rotated, newPrimary, err
}

// PruneKeySet 刪除主要金鑰以外的所有金鑰. 只能在所有密文都改用主要金鑰重新加密之後做，
// 否則用舊金鑰加密的值再也解不開.
func PruneKeySet(keysetBase64 string) (pruned string, removed []uint32, err error) {
	handle, err := readHandle(keysetBase64)
	if err != nil {
		return "", nil, err
	}
	info := handle.KeysetInfo()
	manager := keyset.NewManagerFromHandle(handle)
	for _, k := range info.GetKeyInfo() {
		if k.GetKeyId() == info.GetPrimaryKeyId() {
			continue
		}
		if err := manager.Delete(k.GetKeyId()); err != nil {
			return "", nil, fmt.Errorf("delete key %d: %w", k.GetKeyId(), err)
		}
		removed = append(removed, k.GetKeyId())
	}
	pruned, err = writeManager(manager)
	return pruned, removed, err
}

// DescribeKeySet 列出金鑰集裡的金鑰（ID、是否主要、是否啟用），不含金鑰本身.
func DescribeKeySet(keysetBase64 string) ([]KeyInfo, error) {
	handle, err := readHandle(keysetBase64)
	if err != nil {
		return nil, err
	}
	info := handle.KeysetInfo()
	keys := make([]KeyInfo, 0, len(info.GetKeyInfo()))
	for _, k := range info.GetKeyInfo() {
		keys = append(keys, KeyInfo{
			ID:      k.GetKeyId(),
			Primary: k.GetKeyId() == info.GetPrimaryKeyId(),
			Enabled: k.GetStatus() == tinkpb.KeyStatusType_ENABLED,
			AESGCM:  k.GetTypeUrl() == aesGCMKeyType,
		})
	}
	return keys, nil
}

// PrimaryKeyID 金鑰集的主要金鑰 ID.
func PrimaryKeyID(keysetBase64 string) (uint32, error) {
	handle, err := readHandle(keysetBase64)
	if err != nil {
		return 0, err
	}
	return handle.KeysetInfo().GetPrimaryKeyId(), nil
}

// KeyIDOf 密文（base64）是用哪一把金鑰加密的；不是這個系統產生的密文時 ok 為 false.
// Tink 的密文開頭是 1 個版本位元組（0x01）加上 4 位元組的金鑰 ID.
func KeyIDOf(ciphertextBase64 string) (id uint32, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ciphertextBase64))
	if err != nil || len(raw) < 5 || raw[0] != 0x01 {
		return 0, false
	}
	return binary.BigEndian.Uint32(raw[1:5]), true
}

// errNotAESGCM 金鑰集裡有 AES-GCM 以外的金鑰.
var errNotAESGCM = errors.New("keyset may only contain AES-GCM keys")

// checkAESGCM 只接受 AES-GCM 金鑰.
func checkAESGCM(handle *keyset.Handle) error {
	for _, k := range handle.KeysetInfo().GetKeyInfo() {
		if k.GetTypeUrl() != aesGCMKeyType {
			return fmt.Errorf("%w (key %d is %s)", errNotAESGCM, k.GetKeyId(), k.GetTypeUrl())
		}
	}
	return nil
}

func readHandle(keysetBase64 string) (*keyset.Handle, error) {
	keysetBase64 = strings.TrimSpace(keysetBase64)
	if keysetBase64 == "" {
		return nil, errors.New("empty keyset")
	}
	keysetJSON, err := base64.StdEncoding.DecodeString(keysetBase64)
	if err != nil {
		return nil, fmt.Errorf("decode keyset: %w", err)
	}
	handle, err := insecurecleartextkeyset.Read(keyset.NewJSONReader(bytes.NewReader(keysetJSON)))
	if err != nil {
		return nil, fmt.Errorf("read keyset: %w", err)
	}
	if err := checkAESGCM(handle); err != nil {
		return nil, err
	}
	return handle, nil
}

func writeManager(manager *keyset.Manager) (string, error) {
	handle, err := manager.Handle()
	if err != nil {
		return "", fmt.Errorf("finalize keyset: %w", err)
	}
	var buf bytes.Buffer
	if err := insecurecleartextkeyset.Write(handle, keyset.NewJSONWriter(&buf)); err != nil {
		return "", fmt.Errorf("write keyset: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
