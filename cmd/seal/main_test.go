package main

import (
	"chatbot-go/internal/crypto"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type harness struct {
	t    *testing.T
	home string
}

// do 執行一個指令；input 是管線傳進來的內容（空字串表示從終端機執行）.
func (h *harness) do(input string, args ...string) (string, error) {
	h.t.Helper()
	var out strings.Builder
	err := run(args, &env{
		stdin: strings.NewReader(input), stdout: &out, home: h.home,
		stdinIsTerminal: func() bool { return input == "" },
	})
	return out.String(), err
}

func (h *harness) must(input string, args ...string) string {
	h.t.Helper()
	out, err := h.do(input, args...)
	if err != nil {
		h.t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return out
}

func (h *harness) keysetPath() string {
	return filepath.Join(h.home, ".config", "chatbot-go", "crypto.keyset")
}

func (h *harness) keyset() string {
	h.t.Helper()
	data, err := os.ReadFile(h.keysetPath())
	if err != nil {
		h.t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func lines(s string) []string { return strings.Split(strings.TrimSpace(s), "\n") }

// 完整的金鑰輪替流程：產生 → 加密 → 輪替 → 舊值仍可用 → 重新加密 → 刪除舊金鑰.
func TestKeyRotationWorkflow(t *testing.T) {
	h := &harness{t: t, home: t.TempDir()}

	h.must("", "keyset", "new")
	info, err := os.Stat(h.keysetPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("keyset file mode %v err %v", info.Mode(), err)
	}
	if _, err := h.do("", "keyset", "new"); err == nil || !strings.Contains(err.Error(), "rotate") {
		t.Errorf("a second keyset new must not overwrite the existing one: %v", err)
	}

	old := lines(h.must("channel-secret-value\nchannel-token-value\n", "encrypt"))
	if len(old) != 2 || !strings.HasPrefix(old[0], "ENC(") {
		t.Fatalf("encrypt output = %q", old)
	}
	if out := h.must(strings.Join(old, "\n"), "check"); strings.Count(out, "主要金鑰") != 2 {
		t.Errorf("check before rotation = %s", out)
	}

	h.must("", "keyset", "rotate")
	if out := h.must("", "keyset", "info"); strings.Count(out, "金鑰 ") != 2 || strings.Count(out, "主要") != 1 {
		t.Errorf("info after rotation = %s", out)
	}
	if out := h.must(old[0], "check"); !strings.Contains(out, "舊金鑰") {
		t.Errorf("an old value must be reported as using the old key: %s", out)
	}

	renewed := lines(h.must(strings.Join(old, "\n"), "reencrypt"))
	if out := h.must(strings.Join(renewed, "\n"), "check"); strings.Count(out, "主要金鑰") != 2 {
		t.Errorf("check after reencrypt = %s", out)
	}
	ks := h.keyset()
	for i, want := range []string{"channel-secret-value", "channel-token-value"} {
		got, err := crypto.Decrypt(strings.TrimSuffix(strings.TrimPrefix(renewed[i], "ENC("), ")"), ks)
		if err != nil || got != want {
			t.Errorf("renewed value %d decrypts to %q, %v", i, got, err)
		}
	}

	h.must("", "keyset", "prune")
	if out := h.must("", "keyset", "info"); strings.Count(out, "金鑰 ") != 1 {
		t.Errorf("info after prune = %s", out)
	}
	if out, err := h.do(old[0], "check"); err == nil || !strings.Contains(out, "解不開") {
		t.Errorf("a value under the removed key must fail: %s %v", out, err)
	}
	if out := h.must(renewed[0], "check"); !strings.Contains(out, "主要金鑰") {
		t.Errorf("re-encrypted values keep working after prune: %s", out)
	}
}

// 明文只能用管線輸入；check、reencrypt 的輸出不能出現明文；錯誤格式要擋下.
func TestValuesNeverAppearInOutput(t *testing.T) {
	h := &harness{t: t, home: t.TempDir()}
	h.must("", "keyset", "new")

	if out, err := h.do("", "encrypt"); err == nil || !strings.Contains(err.Error(), "pbpaste") || out != "" {
		t.Errorf("typing a secret at the terminal must be refused: %q %v", out, err)
	}
	sealed := h.must("super-secret-plaintext\n", "encrypt")
	if strings.Contains(sealed, "super-secret-plaintext") {
		t.Fatalf("encrypt echoed the plaintext: %s", sealed)
	}
	for _, cmd := range []string{"check", "reencrypt"} {
		if out := h.must(sealed, cmd); strings.Contains(out, "super-secret-plaintext") {
			t.Errorf("%s printed the plaintext: %s", cmd, out)
		}
	}
	if _, err := h.do(sealed, "encrypt"); err == nil {
		t.Error("encrypting an ENC value twice must be refused")
	}
	if out, err := h.do("plain-text\n", "reencrypt"); err == nil || !strings.Contains(out, "ENC(") {
		t.Errorf("reencrypt needs ENC(...) input: %s %v", out, err)
	}
	if _, err := h.do("x\n", "encrypt", "-keyset", filepath.Join(h.home, "missing.keyset")); err == nil ||
		!strings.Contains(err.Error(), "keyset new") {
		t.Errorf("a missing keyset explains the first step: %v", err)
	}
}
