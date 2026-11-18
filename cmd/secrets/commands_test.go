package main

import (
	"bytes"
	"chatbot-go/internal/crypto"
	"chatbot-go/internal/platform/config"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 本檔的 keyset 與 ENC 值全部在執行期現產（GenerateKeySet / EncryptValue）；只有格式錯誤列（ENC() 之類）是字面值.
// 不用 t.Log 印任何 keyset 或明文.

const (
	sentinelPrefix = "SENTINEL-"
	envToken       = "LINE_CHANNEL_TOKEN"
	envSecret      = "LINE_CHANNEL_SECRET"
)

// fakeClip 是記數的假剪貼簿.
type fakeClip struct {
	content string
	reads   int
	copies  int
	clears  int
	copyErr error
	// lastCopied 是最後一次 Copy 的內容（不受之後的 Clear 影響）.
	lastCopied string
}

func (c *fakeClip) Read() (string, error) {
	c.reads++
	return c.content, nil
}

func (c *fakeClip) Copy(text string) error {
	if c.copyErr != nil {
		return c.copyErr
	}
	c.copies++
	c.content = text
	c.lastCopied = text
	return nil
}

func (c *fakeClip) Clear() error {
	c.clears++
	c.content = ""
	return nil
}

// fakePrompt 依 label 回預先放好的值；未列的 label 回空（= 略過）.
type fakePrompt struct {
	answers map[string][]byte
	labels  []string
}

func (p *fakePrompt) ReadSecret(label string) ([]byte, error) {
	p.labels = append(p.labels, label)
	env, _, _ := strings.Cut(label, " ")
	return append([]byte(nil), p.answers[env]...), nil
}

// scriptedStdin 先給 data、再回一次 EOF（讓 ReadAll 結束），之後每次 Read 視為「使用者按了 Enter」並呼叫 onEnter.
type scriptedStdin struct {
	data    *strings.Reader
	eofSeen bool
	onEnter func()
}

func (s *scriptedStdin) Read(p []byte) (int, error) {
	if s.data.Len() > 0 {
		return s.data.Read(p)
	}
	if !s.eofSeen {
		s.eofSeen = true
		return 0, io.EOF
	}
	if s.onEnter != nil {
		s.onEnter()
	}
	p[0] = '\n'
	return 1, nil
}

// harness 組一次 run 呼叫所需的全部假依賴；enterContent / enterClears 是「使用者按 Enter 當下」的剪貼簿快照.
type harness struct {
	clip         *fakeClip
	prompt       *fakePrompt
	stdout       bytes.Buffer
	stderr       bytes.Buffer
	terminal     bool
	decryptCalls int
	enterSeen    bool
	enterContent string
	enterClears  int
}

func newHarness(clipboard string) *harness {
	return &harness{clip: &fakeClip{content: clipboard}, prompt: &fakePrompt{answers: map[string][]byte{}}}
}

func (h *harness) run(stdin string, args ...string) int {
	in := &scriptedStdin{data: strings.NewReader(stdin), eofSeen: stdin == "", onEnter: func() {
		h.enterSeen = true
		h.enterContent = h.clip.content
		h.enterClears = h.clip.clears
	}}
	return run(args, &deps{
		Clip:       h.clip,
		Prompt:     h.prompt,
		IsTerminal: func() bool { return h.terminal },
		Decrypt: func(enc, ks, path string) (string, bool, error) {
			h.decryptCalls++
			return crypto.DecryptValue(enc, ks, path)
		},
		Stdin:  in,
		Stdout: &h.stdout,
		Stderr: &h.stderr,
	})
}

func (h *harness) out() string { return h.stdout.String() + h.stderr.String() }

func mustKeyset(t *testing.T, n int) string {
	t.Helper()
	ks, err := crypto.GenerateKeySet(n)
	if err != nil {
		t.Fatalf("GenerateKeySet: %v", err)
	}
	return ks
}

func mustRotate(t *testing.T, ks string) string {
	t.Helper()
	rotated, err := crypto.RotateKeySet(ks)
	if err != nil {
		t.Fatalf("RotateKeySet: %v", err)
	}
	return rotated
}

func pathOf(t *testing.T, env string) string {
	t.Helper()
	sf, ok := fieldByEnv(env)
	if !ok {
		t.Fatalf("unknown env %s", env)
	}
	return sf.Path
}

func mustEnc(t *testing.T, ks, env, plain string) string {
	t.Helper()
	enc, err := crypto.EncryptValue(plain, ks, pathOf(t, env))
	if err != nil {
		t.Fatalf("EncryptValue(%s): %v", env, err)
	}
	return enc
}

func mustPrimary(t *testing.T, ks string) uint32 {
	t.Helper()
	id, err := crypto.PrimaryKeyID(ks)
	if err != nil {
		t.Fatalf("PrimaryKeyID: %v", err)
	}
	return id
}

func mustKeyIDs(t *testing.T, ks string) []crypto.KeyInfo {
	t.Helper()
	infos, err := crypto.KeyIDs(ks)
	if err != nil {
		t.Fatalf("KeyIDs: %v", err)
	}
	return infos
}

// assertNoKeysetWindow 斷言 text 不含 keyset 的任一 24 字元視窗（滑動檢查）.
func assertNoKeysetWindow(t *testing.T, label, text, ks string) {
	t.Helper()
	const window = 24
	for i := 0; i+window <= len(ks); i++ {
		if strings.Contains(text, ks[i:i+window]) {
			t.Fatalf("%s leaks a 24-char window of a keyset (offset %d)", label, i)
		}
	}
}

func assertNoSentinel(t *testing.T, label, text string) {
	t.Helper()
	if strings.Contains(text, sentinelPrefix) {
		t.Fatalf("%s contains a plaintext sentinel", label)
	}
}

// tamperENC 把 payload 最後一個 byte 翻轉再編回去；前綴（key id）不變，所以「只看前綴」的實作看不出來.
func tamperENC(t *testing.T, enc string) string {
	t.Helper()
	const marker = "ENC(v1:"
	if !strings.HasPrefix(enc, marker) || !strings.HasSuffix(enc, ")") {
		t.Fatalf("not a v1 ENC value")
	}
	raw, err := base64.StdEncoding.DecodeString(enc[len(marker) : len(enc)-1])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw[len(raw)-1] ^= 0xff
	return marker + base64.StdEncoding.EncodeToString(raw) + ")"
}

// allEnvLines 產 11 行 `ENV=value`；overrides 蓋掉預設，空字串值印成 `ENV=`；skip 內的 env 整行省略.
func allEnvLines(values map[string]string, skip ...string) string {
	var b strings.Builder
	for _, sf := range config.SecretFields {
		omitted := false
		for _, s := range skip {
			omitted = omitted || s == sf.Env
		}
		if omitted {
			continue
		}
		b.WriteString(sf.Env + "=" + values[sf.Env] + "\n")
	}
	return b.String()
}

// encryptedSet 以 ks 為 11 個欄位各產一個 ENC 值（AI 金鑰四欄留空 = 未設）.
func encryptedSet(t *testing.T, ks string) map[string]string {
	t.Helper()
	enc := map[string]string{}
	for _, sf := range config.SecretFields {
		switch sf.Env {
		case "GEMINI_API_KEY", "GROQ_API_KEY", "NVIDIA_API_KEY", "ANTHROPIC_API_KEY":
			enc[sf.Env] = ""
		default:
			enc[sf.Env] = mustEnc(t, ks, sf.Env, sentinelPrefix+strings.ToLower(sf.Env))
		}
	}
	return enc
}

// [F34-41] keyset new：剪貼簿有 5 把合法 keyset、stdout/stderr 不含任一 24 字元視窗、Enter 前不 Clear、Enter 後 Clear 一次.
func TestKeysetNew_CopiesAndNeverPrints(t *testing.T) {
	h := newHarness("")
	if code := h.run("", "keyset", "new"); code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, h.stderr.String())
	}
	if !h.enterSeen {
		t.Fatal("tool never waited for Enter")
	}
	if h.enterClears != 0 {
		t.Fatalf("clipboard cleared %d times before Enter, want 0", h.enterClears)
	}
	ks := h.clip.lastCopied
	assertNoKeysetWindow(t, "stdout+stderr", h.out(), ks)
	if err := crypto.ValidateKeySet(ks); err != nil {
		t.Fatalf("clipboard keyset invalid: %v", err)
	}
	infos := mustKeyIDs(t, ks)
	if len(infos) != 5 {
		t.Fatalf("keys = %d, want 5", len(infos))
	}
	// 真實資料：5 把的 keyset 超過 macOS tty 單行上限 1024，所以只能走剪貼簿.
	if len(ks) <= 1024 {
		t.Fatalf("5-key keyset is %d chars, expected > 1024", len(ks))
	}
	want := "keyset copied (5 keys, primary id " + idString(mustPrimary(t, ks)) + "). " + clearHint + "\n"
	if h.stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", h.stdout.String(), want)
	}
	if h.clip.clears != 1 {
		t.Fatalf("clears after Enter = %d, want 1", h.clip.clears)
	}

	t.Run("keys 3", func(t *testing.T) {
		h3 := newHarness("")
		if code := h3.run("", "keyset", "new", "--keys", "3"); code != exitOK {
			t.Fatalf("exit = %d", code)
		}
		if got := len(mustKeyIDs(t, h3.clip.lastCopied)); got != 3 {
			t.Fatalf("keys = %d, want 3", got)
		}
	})
	for _, bad := range []string{"0", "-1"} {
		t.Run("keys "+bad+" is a usage error", func(t *testing.T) {
			hb := newHarness("")
			if code := hb.run("", "keyset", "new", "--keys", bad); code != exitUsage {
				t.Fatalf("exit = %d, want %d", code, exitUsage)
			}
			if hb.clip.copies != 0 || hb.stdout.Len() != 0 {
				t.Fatal("usage error must not touch clipboard or stdout")
			}
		})
	}
}

// [F34-41] 沒有剪貼簿工具 → exit 1、stdout 空，不退而求其次印 keyset.
func TestKeysetNew_NoClipboardFails(t *testing.T) {
	h := newHarness("")
	h.clip.copyErr = errClipboardUnavailable
	if code := h.run("", "keyset", "new"); code != exitFail {
		t.Fatalf("exit = %d, want %d", code, exitFail)
	}
	if h.stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", h.stdout.String())
	}
	if !strings.Contains(h.stderr.String(), "clipboard unavailable") {
		t.Fatalf("stderr = %q, want the reason", h.stderr.String())
	}
}

// [F34-42] keyset rotate：N+1 把、舊 ENC 仍可解、輸出無 keyset、Enter 後才 Clear；垃圾剪貼簿 exit 1 且不動.
func TestKeysetRotate(t *testing.T) {
	k1 := mustKeyset(t, 3)
	oldEnc := mustEnc(t, k1, envToken, sentinelPrefix+"rotate")
	h := newHarness(k1)
	if code := h.run("", "keyset", "rotate"); code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, h.stderr.String())
	}
	k2 := h.clip.lastCopied
	for _, ks := range []string{k1, k2} {
		assertNoKeysetWindow(t, "stdout+stderr", h.out(), ks)
	}
	if got := len(mustKeyIDs(t, k2)); got != 4 {
		t.Fatalf("rotated keys = %d, want 4", got)
	}
	if mustPrimary(t, k2) == mustPrimary(t, k1) {
		t.Fatal("primary did not change")
	}
	if plain, _, err := crypto.DecryptValue(oldEnc, k2, pathOf(t, envToken)); err != nil || plain != sentinelPrefix+"rotate" {
		t.Fatalf("old ENC no longer decrypts with the rotated keyset: %v", err)
	}
	want := "rotated: 3 -> 4 keys, new primary id " + idString(mustPrimary(t, k2)) + "\n"
	if h.stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", h.stdout.String(), want)
	}
	if h.enterClears != 0 || h.clip.clears != 1 {
		t.Fatalf("clears before/after Enter = %d/%d, want 0/1", h.enterClears, h.clip.clears)
	}

	t.Run("garbage clipboard", func(t *testing.T) {
		hg := newHarness("not-a-keyset")
		if code := hg.run("", "keyset", "rotate"); code != exitFail {
			t.Fatalf("exit = %d, want %d", code, exitFail)
		}
		if hg.clip.content != "not-a-keyset" || hg.clip.copies != 0 || hg.clip.clears != 0 || hg.stdout.Len() != 0 {
			t.Fatal("clipboard must stay untouched and stdout empty")
		}
	})
}

// sentinelAnswers 只替 envs 設隱藏輸入答案，值為 SENTINEL-<env>.
func sentinelAnswers(h *harness, envs ...string) map[string]string {
	plains := map[string]string{}
	for _, env := range envs {
		plains[env] = sentinelPrefix + strings.ToLower(env)
		h.prompt.answers[env] = []byte(plains[env])
	}
	return plains
}

// [F34-42] encrypt --all：三個 sentinel、八個空 → 恰三行，每行以該欄自己的 Path 解得回；輸出無明文與 keyset；Clear 一次.
func TestEncryptAll(t *testing.T) {
	ks := mustKeyset(t, 3)
	h := newHarness(ks)
	plains := sentinelAnswers(h, envToken, "GROQ_API_KEY", "ADMIN_TOKEN")
	if code := h.run("", "encrypt", "--all"); code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, h.stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(h.stdout.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout has %d lines, want 3", len(lines))
	}
	wantOrder := []string{"GROQ_API_KEY", "ADMIN_TOKEN"}
	wantOrder = append([]string{envToken}, wantOrder...)
	for i, line := range lines {
		env, enc, ok := strings.Cut(line, "=")
		if !ok || env != wantOrder[i] || !strings.HasPrefix(enc, "ENC(v1:") || !strings.HasSuffix(enc, ")") {
			t.Fatalf("line %d has the wrong shape (env %q)", i, env)
		}
		got, legacy, err := crypto.DecryptValue(enc, ks, pathOf(t, env))
		if err != nil || legacy || got != plains[env] {
			t.Fatalf("%s does not decrypt with its own field path: %v", env, err)
		}
	}
	if len(h.prompt.labels) != len(config.SecretFields) {
		t.Fatalf("prompts = %d, want %d", len(h.prompt.labels), len(config.SecretFields))
	}
	for i, sf := range config.SecretFields {
		if h.prompt.labels[i] != sf.Env+" (empty = skip):" {
			t.Fatalf("prompt %d = %q", i, h.prompt.labels[i])
		}
	}
	assertNoSentinel(t, "stdout+stderr", h.out())
	assertNoKeysetWindow(t, "stdout+stderr", h.out(), ks)
	if h.clip.clears != 1 {
		t.Fatalf("clears = %d, want 1", h.clip.clears)
	}

	t.Run("single env", func(t *testing.T) {
		hs := newHarness(ks)
		sentinelAnswers(hs, envToken)
		if code := hs.run("", "encrypt", envToken); code != exitOK {
			t.Fatalf("exit = %d", code)
		}
		if len(hs.prompt.labels) != 1 || !strings.HasPrefix(hs.stdout.String(), envToken+"=ENC(v1:") {
			t.Fatalf("single-env encrypt misbehaved: prompts=%d", len(hs.prompt.labels))
		}
	})
}

// [F34-42] encrypt：>= 1000 byte 拒收（不截斷）、未知 ENV 名 exit 2 且不碰剪貼簿/提示.
func TestEncrypt_Rejects(t *testing.T) {
	ks := mustKeyset(t, 2)
	t.Run("1000 bytes rejected not truncated", func(t *testing.T) {
		h := newHarness(ks)
		h.prompt.answers[envToken] = bytes.Repeat([]byte("a"), 1000)
		if code := h.run("", "encrypt", envToken); code != exitFail {
			t.Fatalf("exit = %d, want %d", code, exitFail)
		}
		if h.stdout.Len() != 0 {
			t.Fatal("a truncated or full ENC value was printed")
		}
		if !strings.Contains(h.stderr.String(), envToken) || h.clip.clears != 0 {
			t.Fatalf("stderr = %q clears = %d", h.stderr.String(), h.clip.clears)
		}
	})
	t.Run("999 bytes accepted", func(t *testing.T) {
		h := newHarness(ks)
		h.prompt.answers[envToken] = bytes.Repeat([]byte("a"), 999)
		if code := h.run("", "encrypt", envToken); code != exitOK {
			t.Fatalf("exit = %d, stderr = %q", code, h.stderr.String())
		}
		enc := strings.TrimPrefix(strings.TrimSpace(h.stdout.String()), envToken+"=")
		plain, _, err := crypto.DecryptValue(enc, ks, pathOf(t, envToken))
		if err != nil || len(plain) != 999 {
			t.Fatalf("999-byte value did not round-trip whole: %v", err)
		}
	})
	t.Run("unknown env", func(t *testing.T) {
		h := newHarness(ks)
		if code := h.run("", "encrypt", "NOT_A_FIELD"); code != exitUsage {
			t.Fatalf("exit = %d, want %d", code, exitUsage)
		}
		if h.clip.reads != 0 || len(h.prompt.labels) != 0 || h.stdout.Len() != 0 {
			t.Fatal("unknown env must not read the clipboard or prompt")
		}
	})
}

// [F34-43] reencrypt：舊格式與舊主金鑰的行重加密，現主金鑰的行逐位元組不變；stdout 只有 ENC 行，計數在 stderr.
func TestReencrypt_SkipsCurrent(t *testing.T) {
	k1 := mustKeyset(t, 2)
	k2 := mustRotate(t, k1)
	legacyCT, err := crypto.Encrypt(sentinelPrefix+"legacy", k1)
	if err != nil {
		t.Fatalf("legacy Encrypt: %v", err)
	}
	legacyLine := "REDIS_PASSWORD=ENC(" + legacyCT + ")"
	oldLine := "MONGO_URI=" + mustEnc(t, k1, "MONGO_URI", sentinelPrefix+"old")
	curLine := envToken + "=" + mustEnc(t, k2, envToken, sentinelPrefix+"cur")
	input := []string{legacyLine, oldLine, curLine}

	h := newHarness(k2)
	if code := h.run(strings.Join(input, "\n")+"\n", "reencrypt"); code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, h.stderr.String())
	}
	out := strings.Split(strings.TrimSuffix(h.stdout.String(), "\n"), "\n")
	if len(out) != len(input) {
		t.Fatalf("stdout lines = %d, want %d", len(out), len(input))
	}
	if strings.Contains(h.stdout.String(), "processed=") {
		t.Fatal("counts must go to stderr, not stdout")
	}
	if out[2] != curLine {
		t.Fatal("line already on the primary key must be byte-identical")
	}
	wantPlain := []string{sentinelPrefix + "legacy", sentinelPrefix + "old"}
	for i := range 2 {
		env, enc, _ := strings.Cut(out[i], "=")
		wantEnv, _, _ := strings.Cut(input[i], "=")
		if env != wantEnv || out[i] == input[i] {
			t.Fatalf("line %d was not re-encrypted in place", i)
		}
		id, err := crypto.CiphertextKeyID(enc)
		if err != nil || id != mustPrimary(t, k2) {
			t.Fatalf("line %d is not on the primary key: %v", i, err)
		}
		if got, legacy, err := crypto.DecryptValue(enc, k2, pathOf(t, env)); err != nil || legacy || got != wantPlain[i] {
			t.Fatalf("line %d does not decrypt back to the original: %v", i, err)
		}
	}
	if !strings.Contains(h.stderr.String(), "reencrypt: processed=2 skipped=1") {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
	assertNoSentinel(t, "stdout+stderr", h.out())
	assertNoKeysetWindow(t, "stdout+stderr", h.out(), k2)
	if h.clip.clears != 1 {
		t.Fatalf("clears = %d, want 1", h.clip.clears)
	}
}

// [F34-43] reencrypt 任一行錯 → exit 1、stdout 空、stderr 只含 ENV 名與原因類別、剪貼簿不動.
func TestReencrypt_AllOrNothing(t *testing.T) {
	k1 := mustKeyset(t, 2)
	k2 := mustRotate(t, k1)
	other := mustKeyset(t, 2)
	good := "MONGO_URI=" + mustEnc(t, k1, "MONGO_URI", sentinelPrefix+"good")
	cases := []struct {
		name, bad, wantInErr string
	}{
		{"tampered", envToken + "=" + tamperENC(t, mustEnc(t, k1, envToken, sentinelPrefix+"t")), envToken + ": authentication failed"},
		{"wrong keyset", envToken + "=" + mustEnc(t, other, envToken, sentinelPrefix+"w"), envToken + ": authentication failed"},
		{"field swapped", envSecret + "=" + mustEnc(t, k1, envToken, sentinelPrefix+"s"), envSecret + ": authentication failed"},
		{"bad format", envToken + "=ENC()", envToken + ": bad format"},
		{"unsupported version", envToken + "=ENC(v9:AAAA)", envToken + ": unsupported version"},
		{"unknown env", "NOT_A_FIELD=" + mustEnc(t, k1, envToken, sentinelPrefix+"u"), "line 2: unknown env"},
		{"no equals sign", "garbage-line", "line 2: bad format"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(k2)
			if code := h.run(good+"\n"+tc.bad+"\n", "reencrypt"); code != exitFail {
				t.Fatalf("exit = %d, want %d", code, exitFail)
			}
			if h.stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty (all-or-nothing)", h.stdout.String())
			}
			if !strings.Contains(h.stderr.String(), tc.wantInErr) {
				t.Fatalf("stderr = %q, want it to contain %q", h.stderr.String(), tc.wantInErr)
			}
			assertNoSentinel(t, "stderr", h.stderr.String())
			assertNoKeysetWindow(t, "stderr", h.stderr.String(), k2)
			if strings.Contains(h.stderr.String(), "ENC(") {
				t.Fatal("stderr must not echo ciphertext")
			}
			if h.clip.clears != 0 || h.clip.content != k2 {
				t.Fatal("clipboard must stay untouched on failure")
			}
		})
	}
}

// [F34-44] keyset status：每把 key 一行、每個 env 一行（版面照 Lead 定案）；每行實解，竄改行（前綴相同）→ exit 1、stdout 空.
func TestKeysetStatus(t *testing.T) {
	k1 := mustKeyset(t, 2)
	k2 := mustRotate(t, k1)
	infos := mustKeyIDs(t, k2)
	oldEnvs := []string{"MONGO_URI", "REDIS_PASSWORD", "CWA_AUTH_KEY"}
	enc := encryptedSet(t, k2)
	for _, env := range oldEnvs {
		enc[env] = mustEnc(t, k1, env, sentinelPrefix+"old-"+env)
	}
	input := allEnvLines(enc)

	h := newHarness(k2)
	if code := h.run(input, "keyset", "status"); code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, h.stderr.String())
	}
	var want strings.Builder
	oldID := mustKeyIDs(t, k1)
	for _, info := range infos {
		used := 0
		switch {
		case info.Primary:
			used = 11 - 4 - 3 // 11 欄減 4 個未設再減 3 個舊 key
		case info.ID == oldID[len(oldID)-1].ID:
			used = 3
		}
		want.WriteString("key " + idString(info.ID) + " primary=" + strconv.FormatBool(info.Primary) + " used=" + strconv.Itoa(used) + "\n")
	}
	for _, sf := range config.SecretFields {
		switch {
		case enc[sf.Env] == "":
			want.WriteString(sf.Env + " unset\n")
		case containsStr(oldEnvs, sf.Env):
			want.WriteString(sf.Env + " key=" + idString(oldID[len(oldID)-1].ID) + "\n")
		default:
			want.WriteString(sf.Env + " key=" + idString(mustPrimary(t, k2)) + "\n")
		}
	}
	if h.stdout.String() != want.String() {
		t.Fatalf("stdout:\n%s\nwant:\n%s", h.stdout.String(), want.String())
	}
	assertNoSentinel(t, "stdout+stderr", h.out())
	assertNoKeysetWindow(t, "stdout+stderr", h.out(), k2)

	t.Run("tampered line with the same prefix", func(t *testing.T) {
		bad := map[string]string{}
		for k, v := range enc {
			bad[k] = v
		}
		bad[envToken] = tamperENC(t, enc[envToken])
		ht := newHarness(k2)
		if code := ht.run(allEnvLines(bad), "keyset", "status"); code != exitFail {
			t.Fatalf("exit = %d, want %d", code, exitFail)
		}
		if ht.stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want empty", ht.stdout.String())
		}
		if !strings.Contains(ht.stderr.String(), envToken+": authentication failed") {
			t.Fatalf("stderr = %q", ht.stderr.String())
		}
	})
}

func containsStr(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// pruneFixture 備好 K1（2 把）、K2 = K1 追加主金鑰（3 把）與全在 K2 主金鑰上的 11 行輸入.
type pruneFixture struct {
	k1, k2  string
	enc     map[string]string
	removed []uint32
}

func newPruneFixture(t *testing.T) pruneFixture {
	t.Helper()
	k1 := mustKeyset(t, 2)
	k2 := mustRotate(t, k1)
	enc := encryptedSet(t, k2)
	var removed []uint32
	for _, info := range mustKeyIDs(t, k2) {
		if !info.Primary {
			removed = append(removed, info.ID)
		}
	}
	return pruneFixture{k1: k1, k2: k2, enc: enc, removed: removed}
}

func (f pruneFixture) withEnc(env, value string) map[string]string {
	out := map[string]string{}
	for k, v := range f.enc {
		out[k] = v
	}
	out[env] = value
	return out
}

// [F34-45] prune 成功：剪貼簿是 K3（少兩把、primary 不變、解得開全部非空行）、版面照 Lead 定案、Enter 前不 Clear、Enter 後 Clear 一次.
func TestKeysetPrune_Success(t *testing.T) {
	f := newPruneFixture(t)
	h := newHarness(f.k2)
	if code := h.run(allEnvLines(f.enc), "keyset", "prune"); code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, h.stderr.String())
	}
	if !h.enterSeen {
		t.Fatal("tool never waited for Enter")
	}
	k3 := h.clip.lastCopied
	if err := crypto.ValidateKeySet(k3); err != nil {
		t.Fatalf("K3 invalid: %v", err)
	}
	if mustPrimary(t, k3) != mustPrimary(t, f.k2) {
		t.Fatal("primary changed")
	}
	if got := len(mustKeyIDs(t, k3)); got != len(mustKeyIDs(t, f.k2))-len(f.removed) {
		t.Fatalf("K3 has %d keys", got)
	}
	for _, sf := range config.SecretFields {
		if f.enc[sf.Env] == "" {
			continue
		}
		if _, _, err := crypto.DecryptValue(f.enc[sf.Env], k3, sf.Path); err != nil {
			t.Fatalf("K3 cannot decrypt %s: %v", sf.Env, err)
		}
	}
	ids := make([]string, len(f.removed))
	for i, id := range f.removed {
		ids[i] = idString(id)
	}
	want := "pruned: 3 -> 1 keys, removed " + strings.Join(ids, ",") + ". " + clearHint + "\n"
	if h.stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", h.stdout.String(), want)
	}
	assertNoKeysetWindow(t, "stdout+stderr", h.out(), f.k2)
	assertNoKeysetWindow(t, "stdout+stderr", h.out(), k3)
	if h.enterClears != 0 {
		t.Fatalf("clipboard cleared %d times before Enter, want 0", h.enterClears)
	}
	if h.clip.clears != 1 {
		t.Fatalf("clears after Enter = %d, want 1", h.clip.clears)
	}

	t.Run("explicit --key removes just that key", func(t *testing.T) {
		hk := newHarness(f.k2)
		if code := hk.run(allEnvLines(f.enc), "keyset", "prune", "--key", idString(f.removed[0])); code != exitOK {
			t.Fatalf("exit = %d, stderr = %q", code, hk.stderr.String())
		}
		if got := len(mustKeyIDs(t, hk.clip.lastCopied)); got != 2 {
			t.Fatalf("keys = %d, want 2", got)
		}
	})
}

// assertPruneUntouched 斷言 prune 被拒時剪貼簿原封不動、stdout 空、stderr 不含 keyset 與明文.
func assertPruneUntouched(t *testing.T, h *harness, k2 string) {
	t.Helper()
	if h.stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", h.stdout.String())
	}
	if h.clip.content != k2 || h.clip.copies != 0 || h.clip.clears != 0 {
		t.Fatal("clipboard must stay untouched when prune is refused")
	}
	assertNoKeysetWindow(t, "stderr", h.stderr.String(), k2)
	assertNoSentinel(t, "stderr", h.stderr.String())
}

// [F34-45] 仍有一行依賴舊 key → 拒絕，stderr 含該 env 名與 key id、不含其他 env 名；前綴相同但密文竄改也必須被拒（證明實解每一行）.
func TestKeysetPrune_RefusesInUse(t *testing.T) {
	f := newPruneFixture(t)
	t.Run("one line still on an old key", func(t *testing.T) {
		oldID := idString(mustPrimary(t, f.k1))
		input := allEnvLines(f.withEnc(envToken, mustEnc(t, f.k1, envToken, sentinelPrefix+"old")))
		h := newHarness(f.k2)
		if code := h.run(input, "keyset", "prune"); code != exitFail {
			t.Fatalf("exit = %d, want %d", code, exitFail)
		}
		assertPruneUntouched(t, h, f.k2)
		if !strings.Contains(h.stderr.String(), envToken) || !strings.Contains(h.stderr.String(), oldID) {
			t.Fatalf("stderr = %q, want the env name and key id %s", h.stderr.String(), oldID)
		}
		for _, sf := range config.SecretFields {
			if sf.Env != envToken && strings.Contains(h.stderr.String(), sf.Env) {
				t.Fatalf("stderr names %s, which does not depend on the removed key", sf.Env)
			}
		}
	})
	t.Run("same prefix but tampered ciphertext", func(t *testing.T) {
		input := allEnvLines(f.withEnc(envToken, tamperENC(t, f.enc[envToken])))
		h := newHarness(f.k2)
		if code := h.run(input, "keyset", "prune"); code != exitFail {
			t.Fatalf("exit = %d, want %d", code, exitFail)
		}
		assertPruneUntouched(t, h, f.k2)
		if !strings.Contains(h.stderr.String(), envToken+": authentication failed") {
			t.Fatalf("stderr = %q", h.stderr.String())
		}
	})
}

// [F34-45] 缺一個 env 名 → 拒絕（exit 2），stderr 點出缺的 env 名，剪貼簿不動.
func TestKeysetPrune_RefusesIncomplete(t *testing.T) {
	f := newPruneFixture(t)
	h := newHarness(f.k2)
	if code := h.run(allEnvLines(f.enc, "CWA_AUTH_KEY"), "keyset", "prune"); code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	assertPruneUntouched(t, h, f.k2)
	if !strings.Contains(h.stderr.String(), "CWA_AUTH_KEY") {
		t.Fatalf("stderr = %q, want the missing env name", h.stderr.String())
	}
}

// [F34-45] 含明文行 → exit 1，明文不進 stderr，剪貼簿不動.
func TestKeysetPrune_RefusesPlaintext(t *testing.T) {
	f := newPruneFixture(t)
	h := newHarness(f.k2)
	if code := h.run(allEnvLines(f.withEnc(envToken, sentinelPrefix+"plain-token")), "keyset", "prune"); code != exitFail {
		t.Fatalf("exit = %d, want %d", code, exitFail)
	}
	assertPruneUntouched(t, h, f.k2)
	if !strings.Contains(h.stderr.String(), envToken) {
		t.Fatalf("stderr = %q, want the env name", h.stderr.String())
	}
}

// [F34-45] 含未知 env 名（或重複行）→ exit 2，剪貼簿不動.
func TestKeysetPrune_RefusesUnknown(t *testing.T) {
	f := newPruneFixture(t)
	for name, input := range map[string]string{
		"unknown env":    allEnvLines(f.enc) + "NOT_A_FIELD=" + f.enc[envToken] + "\n",
		"duplicate line": allEnvLines(f.enc) + envToken + "=" + f.enc[envToken] + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(f.k2)
			if code := h.run(input, "keyset", "prune"); code != exitUsage {
				t.Fatalf("exit = %d, want %d", code, exitUsage)
			}
			assertPruneUntouched(t, h, f.k2)
		})
	}
}

// [F34-45] --key 指 primary、不存在的 id → exit 1；不是十進位數字 → exit 2；三者剪貼簿都不動、stdout 空.
func TestKeysetPrune_RejectsBadKey(t *testing.T) {
	f := newPruneFixture(t)
	primary := idString(mustPrimary(t, f.k2))
	missing := "4294967295"
	for _, info := range mustKeyIDs(t, f.k2) {
		if idString(info.ID) == missing {
			t.Skip("generated key collides with the probe id") // 機率可忽略；碰到就明確略過而非誤判.
		}
	}
	cases := []struct {
		name, key string
		want      int
	}{
		{"primary", primary, exitFail},
		{"missing id", missing, exitFail},
		{"not a number", "abc", exitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(f.k2)
			if code := h.run(allEnvLines(f.enc), "keyset", "prune", "--key", tc.key); code != tc.want {
				t.Fatalf("exit = %d, want %d (stderr %q)", code, tc.want, h.stderr.String())
			}
			assertPruneUntouched(t, h, f.k2)
		})
	}
}

// decryptFixture 備好終端機模式下 decrypt LINE_CHANNEL_TOKEN 所需的 keyset 與 ENC 值.
func decryptFixture(t *testing.T) (ks, enc, plain string) {
	t.Helper()
	ks = mustKeyset(t, 2)
	plain = sentinelPrefix + "decrypt-token"
	return ks, mustEnc(t, ks, envToken, plain), plain
}

// [F34-46] 終端機模式 decrypt：stdout 恰為「原值\n」，stderr 空，剪貼簿 Clear；兩種輸入形式（裸 ENC、NAME=ENC）.
func TestDecrypt_TerminalPrintsValue(t *testing.T) {
	ks, enc, plain := decryptFixture(t)
	for name, input := range map[string]string{"bare ENC": enc + "\n", "NAME=ENC": envToken + "=" + enc + "\n"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(ks)
			h.terminal = true
			if code := h.run(input, "decrypt", envToken); code != exitOK {
				t.Fatalf("exit = %d, stderr = %q", code, h.stderr.String())
			}
			if h.stdout.String() != plain+"\n" {
				t.Fatalf("stdout = %q, want exactly the value and a newline", h.stdout.String())
			}
			if h.stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", h.stderr.String())
			}
			if h.clip.clears != 1 {
				t.Fatalf("clears = %d, want 1", h.clip.clears)
			}
		})
	}
}

// [F34-46] 非終端機 decrypt：exit 1、解密函式一次都沒被呼叫（spy）、剪貼簿沒被讀、輸出沒有原值.
func TestDecrypt_NonTerminalRefuses(t *testing.T) {
	ks, enc, plain := decryptFixture(t)
	h := newHarness(ks)
	h.terminal = false
	if code := h.run(enc+"\n", "decrypt", envToken); code != exitFail {
		t.Fatalf("exit = %d, want %d", code, exitFail)
	}
	if h.decryptCalls != 0 {
		t.Fatalf("decrypt function called %d times, want 0", h.decryptCalls)
	}
	if h.clip.reads != 0 {
		t.Fatalf("clipboard read %d times, want 0", h.clip.reads)
	}
	if strings.Contains(h.out(), plain) {
		t.Fatal("plaintext leaked on a non-terminal")
	}
	if h.stdout.Len() != 0 || h.stderr.String() != "decrypt "+envToken+": need a terminal\n" {
		t.Fatalf("stdout = %q stderr = %q", h.stdout.String(), h.stderr.String())
	}
}

// [F34-46] decrypt 失敗：exit 1、stdout 空、stderr 恰為 `decrypt <ENV>: <原因類別>`；未知 ENV exit 2.
func TestDecrypt_FailuresPrintNothing(t *testing.T) {
	ks, enc, plain := decryptFixture(t)
	other := mustKeyset(t, 2)
	cases := []struct {
		name, clip, input, category string
	}{
		{"tampered", ks, tamperENC(t, enc), "authentication failed"},
		{"unsupported version", ks, "ENC(v9:AAAA)", "unsupported version"},
		{"field swapped", ks, mustEnc(t, ks, envSecret, plain), "authentication failed"},
		{"wrong keyset", other, enc, "authentication failed"},
		{"empty clipboard", "", enc, "no keyset"},
		{"clipboard is not a keyset", "not-a-keyset", enc, "no keyset"},
		{"bad format", ks, "ENC()", "bad format"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(tc.clip)
			h.terminal = true
			if code := h.run(tc.input+"\n", "decrypt", envToken); code != exitFail {
				t.Fatalf("exit = %d, want %d", code, exitFail)
			}
			if h.stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", h.stdout.String())
			}
			if want := "decrypt " + envToken + ": " + tc.category + "\n"; h.stderr.String() != want {
				t.Fatalf("stderr = %q, want %q", h.stderr.String(), want)
			}
			if strings.Contains(h.out(), plain) {
				t.Fatal("plaintext leaked on failure")
			}
		})
	}
	t.Run("unknown env", func(t *testing.T) {
		h := newHarness(ks)
		h.terminal = true
		if code := h.run(enc+"\n", "decrypt", "NOT_A_FIELD"); code != exitUsage {
			t.Fatalf("exit = %d, want %d", code, exitUsage)
		}
		if h.stdout.Len() != 0 {
			t.Fatal("stdout must be empty")
		}
	})
}

// [F34-47] decrypt 以外所有子命令在假終端機=true 下，stdout/stderr 仍不含明文 sentinel 與 keyset.
func TestOtherCommands_NeverPrintPlaintextEvenOnTerminal(t *testing.T) {
	f := newPruneFixture(t)
	oldLines := map[string]string{}
	for k, v := range f.enc {
		oldLines[k] = v
	}
	oldLines[envToken] = mustEnc(t, f.k1, envToken, sentinelPrefix+"old")
	allKeysets := []string{f.k1, f.k2}

	cases := []struct {
		name  string
		clip  string
		stdin string
		args  []string
		setup func(*harness)
	}{
		{"keyset new", "", "", []string{"keyset", "new"}, nil},
		{"keyset rotate", f.k2, "", []string{"keyset", "rotate"}, nil},
		{"encrypt --all", f.k2, "", []string{"encrypt", "--all"}, func(h *harness) { sentinelAnswers(h, envToken, "GROQ_API_KEY") }},
		{"encrypt one", f.k2, "", []string{"encrypt", envToken}, func(h *harness) { sentinelAnswers(h, envToken) }},
		{"reencrypt", f.k2, envToken + "=" + oldLines[envToken] + "\n", []string{"reencrypt"}, nil},
		{"keyset status", f.k2, allEnvLines(oldLines), []string{"keyset", "status"}, nil},
		{"keyset prune", f.k2, allEnvLines(f.enc), []string{"keyset", "prune"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(tc.clip)
			h.terminal = true
			if tc.setup != nil {
				tc.setup(h)
			}
			if code := h.run(tc.stdin, tc.args...); code != exitOK {
				t.Fatalf("exit = %d, stderr = %q", code, h.stderr.String())
			}
			assertNoSentinel(t, "stdout+stderr", h.out())
			for _, ks := range allKeysets {
				assertNoKeysetWindow(t, "stdout+stderr", h.out(), ks)
			}
			if h.enterSeen {
				assertNoKeysetWindow(t, "stdout+stderr", h.out(), h.enterContent)
			}
		})
	}
}

// [F34-46] 真實 binary 在 stdout 導向檔案時自己拒絕：exit 1、stderr 說明要終端機，不是 go run 找不到套件.
func TestDecrypt_Binary_NonTerminalSmoke(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	outFile, err := os.Create(filepath.Join(t.TempDir(), "stdout.txt"))
	if err != nil {
		t.Fatalf("create stdout file: %v", err)
	}
	t.Cleanup(func() {
		if err := outFile.Close(); err != nil {
			t.Errorf("close stdout file: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", "./cmd/secrets", "decrypt", envToken)
	cmd.Dir = root // go test 的工作目錄是 cmd/secrets，不設會找不到 ./cmd/secrets.
	cmd.Stdout = outFile
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != exitFail {
		t.Fatalf("run error = %v, want exit code %d", runErr, exitFail)
	}
	text := stderr.String()
	if !strings.Contains(text, "need a terminal") {
		t.Fatalf("stderr = %q, want it to say a terminal is needed", text)
	}
	for _, bad := range []string{"directory", "no such file", "cannot find", "build failed"} {
		if strings.Contains(text, bad) {
			t.Fatalf("stderr mentions %q: go run failed instead of the binary refusing", bad)
		}
	}
	if info, statErr := outFile.Stat(); statErr != nil || info.Size() != 0 {
		t.Fatal("stdout file must stay empty")
	}
}
