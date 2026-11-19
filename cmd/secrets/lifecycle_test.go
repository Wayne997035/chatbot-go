package main

import (
	"bytes"
	"chatbot-go/internal/platform/config"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本檔的 keyset、明文與 ENC 值全部在執行期現產；不用 t.Log 印任何機密.

const msgNotPrimary = `msg="config secrets not on primary key"`

// lifecyclePlain 是 11 個欄位的測試明文（ADMIN_TOKEN 要 >= 32 字元才過 production 驗證）.
func lifecyclePlain() map[string]string {
	plain := map[string]string{}
	for _, sf := range config.SecretFields {
		plain[sf.Env] = sentinelPrefix + strings.ToLower(sf.Env)
	}
	plain["ADMIN_TOKEN"] = sentinelPrefix + "admin-token-0123456789abcdef0123"
	return plain
}

// useProductionConfigs 以符號連結指向真實 configs/，切過去並設 APP_ENV=production；11 個機敏 env 一律先 t.Setenv 清空.
func useProductionConfigs(t *testing.T) {
	t.Helper()
	realConfigs, err := filepath.Abs(filepath.Join("..", "..", "configs"))
	if err != nil {
		t.Fatalf("abs configs dir: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink(realConfigs, filepath.Join(dir, "configs")); err != nil {
		t.Fatalf("symlink configs: %v", err)
	}
	t.Chdir(dir)
	t.Setenv("APP_ENV", "production")
	for _, sf := range config.SecretFields {
		t.Setenv(sf.Env, "")
	}
	t.Setenv("CRYPTO_KEYSET", "")
	t.Setenv("REQUIRE_ENCRYPTED_SECRETS", "")
}

// loadWith 把 lines（env → ENC）與 keyset 放進環境後 config.Load，回傳設定與擷取到的 slog 文字.
// 擷取 slog.Default 的測試不能 t.Parallel；t.Cleanup 還原.
func loadWith(t *testing.T, ks string, lines map[string]string) (*config.Config, string, error) {
	t.Helper()
	for _, sf := range config.SecretFields {
		t.Setenv(sf.Env, lines[sf.Env])
	}
	t.Setenv("CRYPTO_KEYSET", ks)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	c, err := config.Load()
	return c, buf.String(), err
}

func loadedValues(c *config.Config) map[string]string {
	return map[string]string{
		"MONGO_URI":           c.Database.Mongo.URI,
		"REDIS_ADDR":          c.Database.Redis.Addr,
		"REDIS_PASSWORD":      c.Database.Redis.Password,
		"LINE_CHANNEL_SECRET": c.Line.ChannelSecret,
		"LINE_CHANNEL_TOKEN":  c.Line.ChannelToken,
		"CWA_AUTH_KEY":        c.Weather.CWAAuthKey,
		"GEMINI_API_KEY":      c.AI.GeminiAPIKey,
		"GROQ_API_KEY":        c.AI.GroqAPIKey,
		"NVIDIA_API_KEY":      c.AI.NVIDIAAPIKey,
		"ANTHROPIC_API_KEY":   c.AI.AnthropicAPIKey,
		"ADMIN_TOKEN":         c.Security.AdminToken,
	}
}

// mustLoadOK 載入並逐欄比對原文，回傳 slog 文字供 Warn 斷言.
func mustLoadOK(t *testing.T, step, ks string, lines, plain map[string]string) string {
	t.Helper()
	c, logs, err := loadWith(t, ks, lines)
	if err != nil {
		t.Fatalf("%s: Load failed: %v", step, err)
	}
	for env, want := range plain {
		if got := loadedValues(c)[env]; got != want {
			t.Fatalf("%s: %s did not load back to its original value", step, env)
		}
	}
	return logs
}

// parseEnvOutput 把工具 stdout 的 `ENV=ENC(...)` 行轉成 map.
func parseEnvOutput(t *testing.T, out string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		env, enc, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed output line")
		}
		m[env] = enc
	}
	return m
}

// [F34-48] 完整輪替生命週期：K1 加密 11 欄 → rotate → reencrypt → status → prune → K3 Load；回退用 K2 仍可 Load.
func TestRotationLifecycle_EndToEnd(t *testing.T) {
	useProductionConfigs(t)
	plain := lifecyclePlain()
	k1 := mustKeyset(t, 2)

	// 步驟 0：以工具本身把 11 個值加密成 K1 的 ENC 行.
	enc := newHarness(k1)
	for env, value := range plain {
		enc.prompt.answers[env] = []byte(value)
	}
	if code := enc.run("", "encrypt", "--all"); code != exitOK {
		t.Fatalf("encrypt --all exit = %d, stderr = %q", code, enc.stderr.String())
	}
	oldLines := parseEnvOutput(t, enc.stdout.String())
	if len(oldLines) != len(config.SecretFields) {
		t.Fatalf("encrypt produced %d lines, want %d", len(oldLines), len(config.SecretFields))
	}
	logs := mustLoadOK(t, "K1 baseline", k1, oldLines, plain)
	if strings.Contains(logs, msgNotPrimary) {
		t.Fatal("K1 baseline: values are on the primary key, no Warn expected")
	}

	// 步驟 1：rotate → K2.
	rot := newHarness(k1)
	if code := rot.run("", "keyset", "rotate"); code != exitOK {
		t.Fatalf("rotate exit = %d", code)
	}
	k2 := rot.clip.lastCopied
	logs = mustLoadOK(t, "K2 with old values", k2, oldLines, plain)
	if !strings.Contains(logs, msgNotPrimary) {
		t.Fatal("K2 with old values: expected the `config secrets not on primary key` Warn")
	}

	// 步驟 2：reencrypt；之後 Warn 消失.
	re := newHarness(k2)
	if code := re.run(allEnvLines(oldLines), "reencrypt"); code != exitOK {
		t.Fatalf("reencrypt exit = %d, stderr = %q", code, re.stderr.String())
	}
	newLines := parseEnvOutput(t, re.stdout.String())
	logs = mustLoadOK(t, "K2 after reencrypt", k2, newLines, plain)
	if strings.Contains(logs, msgNotPrimary) {
		t.Fatal("K2 after reencrypt: the not-on-primary Warn must be gone")
	}

	// 步驟 3：status 顯示舊 key used=0.
	st := newHarness(k2)
	if code := st.run(allEnvLines(newLines), "keyset", "status"); code != exitOK {
		t.Fatalf("status exit = %d, stderr = %q", code, st.stderr.String())
	}
	for _, info := range mustKeyIDs(t, k2) {
		if !info.Primary && !strings.Contains(st.stdout.String(), "key "+idString(info.ID)+" primary=false used=0\n") {
			t.Fatalf("status must show used=0 for old key %s", idString(info.ID))
		}
	}

	// 步驟 4：prune 前沒 reencrypt 的值會被拒（回退點：剪貼簿與 K2 都沒動）.
	refused := newHarness(k2)
	if code := refused.run(allEnvLines(oldLines), "keyset", "prune"); code != exitFail {
		t.Fatalf("prune with un-reencrypted lines exit = %d, want %d", code, exitFail)
	}
	if refused.clip.content != k2 || refused.clip.copies != 0 {
		t.Fatal("refused prune must leave the clipboard on K2")
	}

	// 步驟 5：以 reencrypt 後的行 prune → K3.
	pr := newHarness(k2)
	if code := pr.run(allEnvLines(newLines), "keyset", "prune"); code != exitOK {
		t.Fatalf("prune exit = %d, stderr = %q", code, pr.stderr.String())
	}
	k3 := pr.clip.lastCopied
	logs = mustLoadOK(t, "K3", k3, newLines, plain)
	if strings.Contains(logs, msgNotPrimary) {
		t.Fatal("K3: no not-on-primary Warn expected")
	}

	// 回退：Render 改回 K2，新舊兩批值都仍可 Load.
	mustLoadOK(t, "rollback K2 + new values", k2, newLines, plain)
	mustLoadOK(t, "rollback K2 + original values", k2, oldLines, plain)
}
