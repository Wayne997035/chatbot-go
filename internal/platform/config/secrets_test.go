package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"chatbot-go/internal/crypto"

	"gopkg.in/yaml.v3"
)

// 本檔的 keyset 與 ENC 值全部在執行期現產（GenerateKeySet / EncryptValue），不寫任何字面值；也不用 t.Log 印任何機密.

const (
	msgPlaintext  = `msg="config has plaintext secrets"`
	msgKeysetGone = `config keyset is empty`
	msgNotPrimary = `msg="config secrets not on primary key"`
	msgLegacy     = `msg="config field uses legacy ENC format, run cmd/secrets reencrypt"`
)

// requiredPlain 是 production 必要的 7 個機敏欄位（AI 金鑰為選填，預設留空）.
func requiredPlain() map[string]string {
	return map[string]string{
		"MONGO_URI":           "mongodb://u:SENTINEL-mongo@host:27017/db",
		"REDIS_ADDR":          "SENTINEL-redis-host:6379",
		"REDIS_PASSWORD":      "SENTINEL-redis-pw",
		"LINE_CHANNEL_SECRET": "SENTINEL-line-secret",
		"LINE_CHANNEL_TOKEN":  "SENTINEL-line-token",
		"CWA_AUTH_KEY":        "SENTINEL-cwa-key",
		"ADMIN_TOKEN":         "SENTINEL-admin-token-0123456789abcdef0123",
	}
}

func newKeyset(t *testing.T, n int) string {
	t.Helper()
	ks, err := crypto.GenerateKeySet(n)
	if err != nil {
		t.Fatalf("GenerateKeySet: %v", err)
	}
	return ks
}

func pathOf(t *testing.T, env string) string {
	t.Helper()
	for _, sf := range SecretFields {
		if sf.Env == env {
			return sf.Path
		}
	}
	t.Fatalf("unknown secret env %q", env)
	return ""
}

// encryptAs 以 keyset 為某個 env 的欄位路徑加密.
func encryptAs(t *testing.T, ks, env, plain string) string {
	t.Helper()
	enc, err := crypto.EncryptValue(plain, ks, pathOf(t, env))
	if err != nil {
		t.Fatalf("EncryptValue(%s): %v", env, err)
	}
	return enc
}

// buildEnv 把 plains 每個欄位以 SecretFields 的路徑加密；plainEnvs 內的保持明文.
func buildEnv(t *testing.T, ks string, plains map[string]string, plainEnvs ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, sf := range SecretFields {
		plain, ok := plains[sf.Env]
		if !ok {
			continue
		}
		if containsString(plainEnvs, sf.Env) {
			out[sf.Env] = plain
			continue
		}
		enc, err := crypto.EncryptValue(plain, ks, sf.Path)
		if err != nil {
			t.Fatalf("EncryptValue(%s): %v", sf.Env, err)
		}
		out[sf.Env] = enc
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// useProductionYAML 把真實的 configs/production.yaml 複製到暫存目錄並切過去，清掉所有機敏 env，APP_ENV=production.
func useProductionYAML(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "configs", "production.yaml"))
	if err != nil {
		t.Fatalf("read production.yaml: %v", err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "configs", "production.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("APP_ENV", "production")
	for _, sf := range SecretFields {
		t.Setenv(sf.Env, "")
	}
	t.Setenv("CRYPTO_KEYSET", "")
	t.Setenv("REQUIRE_ENCRYPTED_SECRETS", "")
}

func setEnv(t *testing.T, envs map[string]string) {
	t.Helper()
	for k, v := range envs {
		t.Setenv(k, v)
	}
}

func loadCaptured(t *testing.T) (*Config, string, error) {
	t.Helper()
	logs := captureLogs(t)
	c, err := Load()
	return c, logs.String(), err
}

func assertNoSecrets(t *testing.T, text string, plains map[string]string, extra ...string) {
	t.Helper()
	for env, plain := range plains {
		if strings.Contains(text, plain) {
			t.Errorf("output leaks the value of %s", env)
		}
	}
	for _, secret := range extra {
		if secret != "" && strings.Contains(text, secret) {
			t.Error("output leaks a secret passed as extra")
		}
	}
}

// [F34-37] production.yaml 的 admin_token 為 ENC 時被解開，32 字元長度驗證作用在解密後.
func TestLoad_DecryptsAdminToken(t *testing.T) {
	useProductionYAML(t)
	ks := newKeyset(t, 2)
	plains := requiredPlain()
	setEnv(t, buildEnv(t, ks, plains))
	t.Setenv("CRYPTO_KEYSET", ks)

	c, _, err := loadCaptured(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Security.AdminToken != plains["ADMIN_TOKEN"] {
		t.Fatal("admin token was not decrypted")
	}
}

// [F34-37] keyset 本身是 ENC → 啟動錯誤，且檢查在 ValidateKeySet 之前（訊息才會是這句）.
func TestLoad_KeysetItselfENC_Rejected(t *testing.T) {
	ks := newKeyset(t, 2)
	for name, value := range map[string]string{
		"encrypted keyset":  encryptAs(t, ks, "ADMIN_TOKEN", "x"),
		"ENC wrapping KMS":  "ENC(KMS(x))",
		"ENC with spaces":   "  ENC(v1:AAAA)  ",
		"bare ENC brackets": "ENC()",
	} {
		t.Run(name, func(t *testing.T) {
			useProductionYAML(t)
			setEnv(t, buildEnv(t, ks, requiredPlain()))
			t.Setenv("CRYPTO_KEYSET", value)
			_, _, err := loadCaptured(t)
			if err == nil || !strings.Contains(err.Error(), "security.keyset must not be ENC") {
				t.Fatalf("err = %v, want security.keyset must not be ENC", err)
			}
		})
	}
}

// [F34-37] 欄位互換：A 欄位的密文貼到 B 欄位 → 啟動失敗，只帶欄位路徑與原因類別.
func TestLoad_FieldSwapRejected(t *testing.T) {
	ks := newKeyset(t, 2)
	plains := requiredPlain()

	useProductionYAML(t)
	envs := buildEnv(t, ks, plains)
	swapped := encryptAs(t, ks, "LINE_CHANNEL_TOKEN", plains["LINE_CHANNEL_SECRET"])
	envs["LINE_CHANNEL_SECRET"] = swapped
	setEnv(t, envs)
	t.Setenv("CRYPTO_KEYSET", ks)

	_, _, err := loadCaptured(t)
	if err == nil {
		t.Fatal("swapped ciphertext must fail Load")
	}
	for _, want := range []string{"line.channel_secret", "authentication failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "no keyset") || strings.Contains(err.Error(), swapped) {
		t.Errorf("err = %v, must not say no keyset or echo the ciphertext", err)
	}

	// 正對照：為 line.channel_secret 加密的值 → 成功（證明上面的失敗來自 AAD，不是 keyset 或別的欄位）.
	envs["LINE_CHANNEL_SECRET"] = encryptAs(t, ks, "LINE_CHANNEL_SECRET", plains["LINE_CHANNEL_SECRET"])
	setEnv(t, envs)
	if _, _, err := loadCaptured(t); err != nil {
		t.Fatalf("control Load: %v", err)
	}
}

// [F34-38] 7 個必要欄位全 ENC（以 SecretFields 的路徑迴圈加密）、4 個 AI 金鑰空、強制模式開 → 成功、無 plaintext 字樣.
func TestLoad_Production_Enforce_AllENC_Passes(t *testing.T) {
	useProductionYAML(t)
	ks := newKeyset(t, 3)
	plains := requiredPlain()
	setEnv(t, buildEnv(t, ks, plains))
	t.Setenv("CRYPTO_KEYSET", ks)
	t.Setenv("REQUIRE_ENCRYPTED_SECRETS", "true")

	c, logs, err := loadCaptured(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := map[string]string{
		"MONGO_URI":           c.Database.Mongo.URI,
		"REDIS_ADDR":          c.Database.Redis.Addr,
		"REDIS_PASSWORD":      c.Database.Redis.Password,
		"LINE_CHANNEL_SECRET": c.Line.ChannelSecret,
		"LINE_CHANNEL_TOKEN":  c.Line.ChannelToken,
		"CWA_AUTH_KEY":        c.Weather.CWAAuthKey,
		"ADMIN_TOKEN":         c.Security.AdminToken,
	}
	for env, want := range plains {
		if got[env] != want {
			t.Errorf("%s decrypted to a different value", env)
		}
	}
	if strings.Contains(logs, "plaintext") {
		t.Errorf("logs mention plaintext although every secret is ENC: %s", logs)
	}
}

// [F34-38] 警告模式：只列 env 名（依表序），不帶值、不帶路徑+值.
func TestLoad_Production_PlaintextWarnsNamesOnly(t *testing.T) {
	useProductionYAML(t)
	ks := newKeyset(t, 2)
	plains := requiredPlain()
	plains["GROQ_API_KEY"] = "SENTINEL-groq-key"
	setEnv(t, buildEnv(t, ks, plains, "GROQ_API_KEY", "LINE_CHANNEL_TOKEN"))
	t.Setenv("CRYPTO_KEYSET", ks)

	_, logs, err := loadCaptured(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := strings.Count(logs, msgPlaintext); n != 1 {
		t.Fatalf("plaintext warnings = %d, want 1; logs: %s", n, logs)
	}
	// 欄位順序依 SecretFields（1.3 表）：LINE_CHANNEL_TOKEN 在 GROQ_API_KEY 之前.
	if !strings.Contains(logs, "fields=LINE_CHANNEL_TOKEN,GROQ_API_KEY") {
		t.Errorf("fields list wrong; logs: %s", logs)
	}
	assertNoSecrets(t, logs, plains)
}

// [F34-38] keyset 空：一則 keyset 警告，且仍列出明文欄位名.
func TestLoad_Production_EmptyKeysetWarns(t *testing.T) {
	useProductionYAML(t)
	plains := requiredPlain()
	delete(plains, "REDIS_ADDR")
	delete(plains, "REDIS_PASSWORD")
	delete(plains, "CWA_AUTH_KEY")
	plains["GEMINI_API_KEY"] = "SENTINEL-gemini-key"
	setEnv(t, plains)

	_, logs, err := loadCaptured(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(logs, msgKeysetGone) {
		t.Errorf("missing keyset-empty warning; logs: %s", logs)
	}
	if strings.Count(logs, msgPlaintext) != 1 || !strings.Contains(logs, "fields=MONGO_URI,LINE_CHANNEL_SECRET,LINE_CHANNEL_TOKEN,GEMINI_API_KEY,ADMIN_TOKEN") {
		t.Errorf("plaintext field list missing or wrong; logs: %s", logs)
	}
	assertNoSecrets(t, logs, plains)
}

// [F34-38] 強制模式 + 有明文 → 啟動錯誤，只列 env 名.
func TestLoad_Production_Enforce_PlaintextFails(t *testing.T) {
	useProductionYAML(t)
	ks := newKeyset(t, 2)
	plains := requiredPlain()
	setEnv(t, buildEnv(t, ks, plains, "LINE_CHANNEL_TOKEN"))
	t.Setenv("CRYPTO_KEYSET", ks)
	t.Setenv("REQUIRE_ENCRYPTED_SECRETS", "true")

	_, _, err := loadCaptured(t)
	if err == nil || !strings.Contains(err.Error(), "plaintext secrets: LINE_CHANNEL_TOKEN") {
		t.Fatalf("err = %v, want plaintext secrets: LINE_CHANNEL_TOKEN", err)
	}
	assertNoSecrets(t, err.Error(), plains)
}

// [F34-38] 強制模式 + keyset 空 → 啟動錯誤（只查明文不查 keyset 會在這列漏掉）.
func TestLoad_Production_Enforce_EmptyKeysetFails(t *testing.T) {
	useProductionYAML(t)
	plains := requiredPlain()
	setEnv(t, plains)
	t.Setenv("REQUIRE_ENCRYPTED_SECRETS", "1")

	_, _, err := loadCaptured(t)
	if err == nil || !strings.Contains(err.Error(), "keyset is empty") {
		t.Fatalf("err = %v, want keyset is empty", err)
	}
	assertNoSecrets(t, err.Error(), plains)
}

// [F34-38] 開關解析與環境範圍：production 的 maybe 啟動失敗且不回顯值；非 production 完全不判不警告.
func TestLoad_Enforce_ParseAndEnvScope(t *testing.T) {
	t.Run("production invalid bool", func(t *testing.T) {
		useProductionYAML(t)
		setEnv(t, requiredPlain())
		t.Setenv("REQUIRE_ENCRYPTED_SECRETS", "maybe")
		_, _, err := loadCaptured(t)
		if err == nil || !strings.Contains(err.Error(), "security.require_encrypted") || strings.Contains(err.Error(), "maybe") {
			t.Fatalf("err = %v, want security.require_encrypted without the raw value", err)
		}
	})

	t.Run("non-production ignores the switch", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o750); err != nil {
			t.Fatal(err)
		}
		yamlText := "database:\n  mongo:\n    uri: ${MONGO_URI}\nline:\n  channel_secret: ${LINE_CHANNEL_SECRET}\n" +
			"  channel_token: ${LINE_CHANNEL_TOKEN}\nsecurity:\n  keyset: ${CRYPTO_KEYSET}\n  require_encrypted: ${REQUIRE_ENCRYPTED_SECRETS}\n"
		if err := os.WriteFile(filepath.Join(dir, "configs", "testenv.yaml"), []byte(yamlText), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		t.Setenv("APP_ENV", "testenv")
		t.Setenv("MONGO_URI", "mongodb://plain-host")
		t.Setenv("LINE_CHANNEL_SECRET", "plain-secret")
		t.Setenv("LINE_CHANNEL_TOKEN", "plain-token")
		t.Setenv("CRYPTO_KEYSET", "")
		t.Setenv("REQUIRE_ENCRYPTED_SECRETS", "true")
		_, logs, err := loadCaptured(t)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if strings.Contains(logs, "plaintext") || strings.Contains(logs, "keyset is empty") {
			t.Errorf("non-production must not warn about secrets; logs: %s", logs)
		}
	})
}

// [F34-37] 舊格式 ENC(<b64>) 仍可讀，但每欄一則 Warn 並提示 reencrypt.
func TestLoad_LegacyFormat_WarnsPerField(t *testing.T) {
	useProductionYAML(t)
	ks := newKeyset(t, 2)
	plains := requiredPlain()
	envs := buildEnv(t, ks, plains)
	for _, env := range []string{"REDIS_PASSWORD", "CWA_AUTH_KEY"} {
		//nolint:staticcheck // SA1019: 舊格式（nil AAD）的測試資料只能用已標 Deprecated 的 Encrypt 產生.
		ct, err := crypto.Encrypt(plains[env], ks)
		if err != nil {
			t.Fatalf("legacy Encrypt: %v", err)
		}
		envs[env] = "ENC(" + ct + ")"
	}
	setEnv(t, envs)
	t.Setenv("CRYPTO_KEYSET", ks)

	c, logs, err := loadCaptured(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := strings.Count(logs, msgLegacy); n != 2 {
		t.Fatalf("legacy warnings = %d, want 2 (one per field); logs: %s", n, logs)
	}
	for _, path := range []string{"database.redis.password", "weather.cwa_auth_key"} {
		if !strings.Contains(logs, "field="+path) {
			t.Errorf("missing legacy warning for %s", path)
		}
	}
	if c.Database.Redis.Password != plains["REDIS_PASSWORD"] {
		t.Error("legacy value was not decrypted")
	}
}

// 環境變數先展開、再解密：ENC 的 base64 含 + / = 也要原封不動.
func TestLoad_ENCInEnvExpandedThenDecrypted(t *testing.T) {
	ks := newKeyset(t, 2)
	var enc, plain string
	for i := range 2000 {
		plain = "SENTINEL-special-" + strings.Repeat("x", i%7)
		enc = encryptAs(t, ks, "MONGO_URI", plain)
		if strings.Contains(enc, "+") && strings.Contains(enc, "/") && strings.Contains(enc, "=") {
			break
		}
		enc = ""
	}
	if enc == "" {
		t.Fatal("could not generate an ENC value containing + / =")
	}
	useProductionYAML(t)
	plains := requiredPlain()
	envs := buildEnv(t, ks, plains)
	envs["MONGO_URI"] = enc
	setEnv(t, envs)
	t.Setenv("CRYPTO_KEYSET", ks)

	c, _, err := loadCaptured(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Database.Mongo.URI != plain {
		t.Fatal("ENC value containing + / = was not decrypted correctly")
	}
}

// [F34-38] 真實 production.yaml：11 個欄位的值都是 ${<Env>}、表內 Path 都解析得到可設定的 string 欄位；
// 看起來像機密的 ${...} 欄位都要在表內（新增第 12 個機密沒進表 → 紅）.
func TestSecretFields_MatchProductionYaml(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "configs", "production.yaml"))
	if err != nil {
		t.Fatalf("read production.yaml: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse production.yaml: %v", err)
	}
	if len(SecretFields) != 11 {
		t.Fatalf("len(SecretFields) = %d, want 11", len(SecretFields))
	}

	inTable := map[string]bool{}
	for _, sf := range SecretFields {
		inTable[sf.Path] = true
		if got := yamlScalar(doc, sf.Path); got != "${"+sf.Env+"}" {
			t.Errorf("production.yaml %s = %q, want ${%s}", sf.Path, got, sf.Env)
		}
		if _, ok := stringFieldByPath(reflectConfig(), sf.Path); !ok {
			t.Errorf("path %s does not resolve to a settable string field", sf.Path)
		}
	}

	secretish := regexp.MustCompile(`(?i)(secret|token|password|api_key|auth_key|uri|addr)`)
	for _, leaf := range yamlLeaves(doc, "") {
		name := leaf.path[strings.LastIndex(leaf.path, ".")+1:]
		if leaf.path == keysetPath || !strings.HasPrefix(leaf.value, "${") || !secretish.MatchString(name) {
			continue
		}
		if !inTable[leaf.path] {
			t.Errorf("%s (%s) looks like a secret but is not in SecretFields", leaf.path, leaf.value)
		}
	}
}

// [F34-38] 帶全部 env 跑真實 production.yaml 的 Load；非機敏 AI 欄位即使填 ENC 也照解，不算明文、不報錯.
func TestLoad_ProductionYaml_Parses(t *testing.T) {
	useProductionYAML(t)
	ks := newKeyset(t, 2)
	plains := requiredPlain()
	envs := buildEnv(t, ks, plains)
	const models = "model-a,model-b"
	envs["NVIDIA_MODELS"] = func() string {
		enc, err := crypto.EncryptValue(models, ks, "ai.nvidia_models")
		if err != nil {
			t.Fatal(err)
		}
		return enc
	}()
	envs["GROQ_MODELS"] = "plain-model"
	envs["AI_ATTEMPT_TIMEOUT_SECONDS"] = "30"
	setEnv(t, envs)
	t.Setenv("CRYPTO_KEYSET", ks)
	t.Setenv("REQUIRE_ENCRYPTED_SECRETS", "true")

	c, logs, err := loadCaptured(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AI.NVIDIAModels != models || c.AI.GroqModels != "plain-model" {
		t.Errorf("non-secret AI fields wrong: nvidia=%q groq=%q", c.AI.NVIDIAModels, c.AI.GroqModels)
	}
	if strings.Contains(logs, "plaintext") {
		t.Errorf("non-secret fields must not count as plaintext; logs: %s", logs)
	}
}

// [F34-39] 值仍在舊金鑰：一則 Warn，只列該 env 名.
func TestLoad_Production_NonPrimaryKeyWarns(t *testing.T) {
	useProductionYAML(t)
	k1 := newKeyset(t, 2)
	k2, err := crypto.RotateKeySet(k1)
	if err != nil {
		t.Fatal(err)
	}
	plains := requiredPlain()
	envs := buildEnv(t, k2, plains)
	for _, env := range []string{"REDIS_PASSWORD", "CWA_AUTH_KEY"} {
		envs[env] = encryptAs(t, k1, env, plains[env])
	}
	setEnv(t, envs)
	t.Setenv("CRYPTO_KEYSET", k2)

	_, logs, err := loadCaptured(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := strings.Count(logs, msgNotPrimary); n != 1 {
		t.Fatalf("not-on-primary warnings = %d, want 1; logs: %s", n, logs)
	}
	if !strings.Contains(logs, "fields=REDIS_PASSWORD,CWA_AUTH_KEY") {
		t.Errorf("fields list wrong; logs: %s", logs)
	}
	assertNoSecrets(t, logs, plains, k1, k2, envs["REDIS_PASSWORD"], envs["CWA_AUTH_KEY"])
}

// [F34-39] 正對照：全部已在主金鑰 → 無該 Warn.
func TestLoad_Production_PrimaryKeyNoWarn(t *testing.T) {
	useProductionYAML(t)
	k1 := newKeyset(t, 2)
	k2, err := crypto.RotateKeySet(k1)
	if err != nil {
		t.Fatal(err)
	}
	setEnv(t, buildEnv(t, k2, requiredPlain()))
	t.Setenv("CRYPTO_KEYSET", k2)

	_, logs, err := loadCaptured(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(logs, msgNotPrimary) {
		t.Errorf("unexpected not-on-primary warning; logs: %s", logs)
	}
}

// [F34-39] 強制模式只管明文與空 keyset，不因舊金鑰失敗（輪替中途部署不能中斷）.
func TestLoad_Enforce_NonPrimaryStillStarts(t *testing.T) {
	useProductionYAML(t)
	k1 := newKeyset(t, 2)
	k2, err := crypto.RotateKeySet(k1)
	if err != nil {
		t.Fatal(err)
	}
	setEnv(t, buildEnv(t, k1, requiredPlain()))
	t.Setenv("CRYPTO_KEYSET", k2)
	t.Setenv("REQUIRE_ENCRYPTED_SECRETS", "true")

	_, logs, err := loadCaptured(t)
	if err != nil {
		t.Fatalf("Load must succeed in enforce mode with old-key values: %v", err)
	}
	if !strings.Contains(logs, msgNotPrimary) {
		t.Errorf("expected the not-on-primary warning; logs: %s", logs)
	}
}

// [F34-36][F34-37] `KMS(` keyset 在警告與強制模式都啟動失敗，不得當空 keyset、不得先 Warn 再啟動.
func TestLoad_Production_KMSPrefixKeysetFailsClosed(t *testing.T) {
	const kmsValue = "KMS(SENTINEL-KS)"
	ks := newKeyset(t, 2)
	plains := requiredPlain()
	for _, require := range []string{"", "true"} {
		for _, mode := range []string{"plaintext fields", "ENC fields"} {
			t.Run("require="+require+"/"+mode, func(t *testing.T) {
				useProductionYAML(t)
				if mode == "ENC fields" {
					setEnv(t, buildEnv(t, ks, plains))
				} else {
					setEnv(t, plains)
				}
				t.Setenv("CRYPTO_KEYSET", kmsValue)
				t.Setenv("REQUIRE_ENCRYPTED_SECRETS", require)
				c, logs, err := loadCaptured(t)
				if err == nil || c != nil {
					t.Fatalf("Load must fail closed on a KMS( keyset (err=%v)", err)
				}
				for _, want := range []string{"security.keyset", "unsupported keyset source"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("err = %v, want it to contain %q", err, want)
					}
				}
				assertNoSecrets(t, err.Error()+logs, plains, "SENTINEL-KS")
				if strings.Contains(logs, "plaintext") {
					t.Errorf("must not warn and continue; logs: %s", logs)
				}
			})
		}
	}
}

func reflectConfig() reflect.Value { return reflect.ValueOf(&Config{}).Elem() }

// yamlScalar 取 yaml 文件裡某個點分路徑的字串值（找不到回空字串）.
func yamlScalar(doc map[string]any, path string) string {
	var cur any = doc
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[part]
	}
	s, _ := cur.(string)
	return s
}

type yamlLeaf struct{ path, value string }

// yamlLeaves 列出所有字串葉節點（點分路徑 → 值）.
func yamlLeaves(node any, prefix string) []yamlLeaf {
	switch n := node.(type) {
	case map[string]any:
		var out []yamlLeaf
		for k, v := range n {
			child := k
			if prefix != "" {
				child = prefix + "." + k
			}
			out = append(out, yamlLeaves(v, child)...)
		}
		return out
	case string:
		return []yamlLeaf{{path: prefix, value: n}}
	}
	return nil
}
