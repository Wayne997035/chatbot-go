package config

import (
	"chatbot-go/internal/crypto"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
)

// SecretField 是一個機敏設定欄位：Env 是 production.yaml 引用的環境變數名，Path 是它在設定檔的 yaml 路徑（同時是 ENC 的 AAD）.
type SecretField struct {
	Env  string
	Path string
}

// SecretFields [F34-38] 機敏欄位表（唯一真相源，與 configs/production.yaml 對齊；cmd/secrets 也讀它）. 順序固定，警告訊息依此序列出.
var SecretFields = []SecretField{
	{Env: "MONGO_URI", Path: "database.mongo.uri"},
	{Env: "REDIS_ADDR", Path: "database.redis.addr"},
	{Env: "REDIS_PASSWORD", Path: "database.redis.password"},
	{Env: "LINE_CHANNEL_SECRET", Path: "line.channel_secret"},
	{Env: "LINE_CHANNEL_TOKEN", Path: "line.channel_token"},
	{Env: "CWA_AUTH_KEY", Path: "weather.cwa_auth_key"},
	{Env: "GEMINI_API_KEY", Path: "ai.gemini_api_key"},
	{Env: "GROQ_API_KEY", Path: "ai.groq_api_key"},
	{Env: "NVIDIA_API_KEY", Path: "ai.nvidia_api_key"},
	{Env: "ANTHROPIC_API_KEY", Path: "ai.anthropic_api_key"},
	{Env: "ADMIN_TOKEN", Path: "security.admin_token"},
}

// keysetPath 是唯一不做 ENC 解密的欄位（keyset 本身不能被自己加密）.
const keysetPath = "security.keyset"

// decryptState 記錄 walk 期間「由 ENC 解出」的欄位：path → 原始 ENC 值（ENC 不是機密，用來查密文前綴的 key id）.
type decryptState struct {
	keyset    string
	encrypted map[string]string
}

// yamlName 回傳欄位在 yaml 的名字（tag 逗號前；沒有 tag 用小寫欄位名，與 yaml.v3 一致）；空字串代表略過.
func yamlName(f *reflect.StructField) string {
	if f.PkgPath != "" {
		return ""
	}
	tag := f.Tag.Get("yaml")
	if tag == "-" {
		return ""
	}
	if name, _, _ := strings.Cut(tag, ","); name != "" {
		return name
	}
	return strings.ToLower(f.Name)
}

// stringFieldByPath 依 yaml 路徑找到可設定的 string 欄位.
func stringFieldByPath(v reflect.Value, path string) (reflect.Value, bool) {
	for _, part := range strings.Split(path, ".") {
		if v.Kind() == reflect.Ptr {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			return reflect.Value{}, false
		}
		found := false
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if yamlName(&field) == part {
				v = v.Field(i)
				found = true
				break
			}
		}
		if !found {
			return reflect.Value{}, false
		}
	}
	return v, v.Kind() == reflect.String && v.CanSet()
}

// parseRequireEncrypted 解析 security.require_encrypted；空字串 = 關；無法解析 = 啟動錯誤（不回顯值）.
func parseRequireEncrypted(raw string) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, nil
	}
	on, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.New("security.require_encrypted: invalid bool")
	}
	return on, nil
}

// plaintextSecrets [F34-38] SecretFields 中值非空、且不是由 ENC 解出的欄位 → 回 env 名（依表序）.
func plaintextSecrets(c *Config, st *decryptState) []string {
	root := reflect.ValueOf(c).Elem()
	var names []string
	for _, sf := range SecretFields {
		field, ok := stringFieldByPath(root, sf.Path)
		if !ok || field.String() == "" {
			continue
		}
		if _, fromENC := st.encrypted[sf.Path]; !fromENC {
			names = append(names, sf.Env)
		}
	}
	return names
}

// checkSecrets 只在 APP_ENV=production 判斷明文、強制開關與非主金鑰.
// [F34-38] 警告模式只 Warn（只列 env 名）；強制模式 keyset 空或有明文 → 回錯（只列 env 名）.
func checkSecrets(c *Config, env string, st *decryptState) error {
	if env != "production" {
		return nil
	}
	enforce, err := parseRequireEncrypted(c.Security.RequireEncrypted)
	if err != nil {
		return err
	}
	keysetEmpty := strings.TrimSpace(c.Security.KeySet) == ""
	plaintext := plaintextSecrets(c, st)

	if enforce {
		if keysetEmpty {
			return errors.New("require_encrypted: keyset is empty")
		}
		if len(plaintext) > 0 {
			return fmt.Errorf("plaintext secrets: %s", strings.Join(plaintext, ","))
		}
	} else {
		if keysetEmpty {
			slog.Warn("config keyset is empty, secrets stay plaintext")
		}
		if len(plaintext) > 0 {
			slog.Warn("config has plaintext secrets", "fields", strings.Join(plaintext, ","))
		}
	}

	if !keysetEmpty {
		warnNotOnPrimary(c.Security.KeySet, st)
	}
	return nil
}

// warnNotOnPrimary [F34-39] 由 ENC 解出的欄位若密文前綴的 key id 不是主金鑰 → 一則 Warn，只列 env 名；不影響啟動（強制模式也一樣）.
func warnNotOnPrimary(keysetBase64 string, st *decryptState) {
	primary, err := crypto.PrimaryKeyID(keysetBase64)
	if err != nil {
		return
	}
	var names []string
	for _, sf := range SecretFields {
		enc, fromENC := st.encrypted[sf.Path]
		if !fromENC {
			continue
		}
		if id, idErr := crypto.CiphertextKeyID(enc); idErr != nil || id != primary {
			names = append(names, sf.Env)
		}
	}
	if len(names) > 0 {
		slog.Warn("config secrets not on primary key", "fields", strings.Join(names, ","))
	}
}
