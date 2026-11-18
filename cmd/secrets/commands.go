package main

import (
	"chatbot-go/internal/crypto"
	"chatbot-go/internal/platform/config"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2

	// maxSecretBytes [F34-42] 隱藏輸入達此長度一律拒收（不截斷）；macOS tty 單行上限約 1024（未驗證對 ReadPassword 是否同樣適用）.
	maxSecretBytes = 1000
	// maxStdinBytes 限制 stdin 讀取量（ENC 行不是機密，但仍不讀無上限）.
	maxStdinBytes = 1 << 20

	clearHint = "Paste to Render CRYPTO_KEYSET and your password manager, then press Enter to clear clipboard."

	usageText = `usage:
  secrets keyset new [--keys N]
  secrets keyset rotate
  secrets keyset status
  secrets keyset prune [--key <id>]
  secrets encrypt --all | <ENV_NAME>
  secrets reencrypt
  secrets decrypt <ENV_NAME>`
)

// Clipboard 是剪貼簿介面；keyset 只經這裡進出.
type Clipboard interface {
	Read() (string, error)
	Copy(text string) error
	Clear() error
}

// Prompter 讀不回顯的隱藏輸入（只用於明文值）.
type Prompter interface {
	ReadSecret(label string) ([]byte, error)
}

// deps [F34-41] 所有外部副作用都由這裡注入，run 才能單元測試.
type deps struct {
	Clip       Clipboard
	Prompt     Prompter
	IsTerminal func() bool
	// Decrypt 為 nil 時用 crypto.DecryptValue；測試注入 spy 以證明 decrypt 在非終端機時根本沒被呼叫.
	Decrypt func(enc, keysetBase64, fieldPath string) (string, bool, error)
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

func (d *deps) decrypt(enc, keysetBase64, fieldPath string) (plain string, legacy bool, err error) {
	if d.Decrypt != nil {
		return d.Decrypt(enc, keysetBase64, fieldPath)
	}
	return crypto.DecryptValue(enc, keysetBase64, fieldPath)
}

// fail 把一行訊息寫到 stderr 並回傳 exit code；呼叫端只能傳入固定文字、env 名、key id、原因類別.
func (d *deps) fail(code int, format string, args ...any) int {
	if _, err := fmt.Fprintf(d.Stderr, format+"\n", args...); err != nil {
		return code
	}
	return code
}

// sayf 寫一行到 stdout.
func (d *deps) sayf(format string, args ...any) {
	if _, err := fmt.Fprintf(d.Stdout, format+"\n", args...); err != nil {
		return
	}
}

// subcommands 把子命令名對到處理函式；run 與 runKeyset 共用同一個派發邏輯.
type subcommands map[string]func(args []string, d *deps) int

func (d *deps) dispatch(args []string, table subcommands, unknown string) int {
	if len(args) > 0 {
		if handler, ok := table[args[0]]; ok {
			return handler(args[1:], d)
		}
	}
	return d.fail(exitUsage, "%s\n%s", unknown, usageText)
}

// run 是整個工具的入口，回傳 exit code.
func run(args []string, d *deps) int {
	return d.dispatch(args, subcommands{
		"keyset":    runKeyset,
		"encrypt":   cmdEncrypt,
		"reencrypt": cmdReencrypt,
		"decrypt":   cmdDecrypt,
	}, "unknown command")
}

func runKeyset(args []string, d *deps) int {
	return d.dispatch(args, subcommands{
		"new":    cmdKeysetNew,
		"rotate": cmdKeysetRotate,
		"status": cmdKeysetStatus,
		"prune":  cmdKeysetPrune,
	}, "unknown keyset command")
}

// ---- 共用小工具 ----

func idString(id uint32) string { return strconv.FormatUint(uint64(id), 10) }

func fieldByEnv(name string) (config.SecretField, bool) {
	for _, sf := range config.SecretFields {
		if sf.Env == name {
			return sf, true
		}
	}
	return config.SecretField{}, false
}

// reasonOf 把 crypto 的錯誤收斂成四類原因；不轉印 crypto 的原始字串.
func reasonOf(err error) string {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "bad format"):
		return "bad format"
	case strings.HasPrefix(msg, "unsupported version"):
		return "unsupported version"
	case strings.HasPrefix(msg, "authentication failed"):
		return "authentication failed"
	default:
		return "no keyset"
	}
}

// readKeyset 從剪貼簿取 keyset 並驗證；錯誤只回固定文字.
func (d *deps) readKeyset() (string, error) {
	raw, err := d.Clip.Read()
	if err != nil {
		return "", errClipboardUnavailable
	}
	ks := strings.TrimSpace(raw)
	if ks == "" {
		return "", errors.New("clipboard is empty")
	}
	if err := crypto.ValidateKeySet(ks); err != nil {
		return "", errors.New("clipboard does not hold a valid keyset")
	}
	return ks, nil
}

// waitEnter 等使用者按 Enter（stdin 讀到 EOF 或錯誤就不等了，避免卡死）.
func (d *deps) waitEnter() {
	buf := make([]byte, 1)
	for {
		n, err := d.Stdin.Read(buf)
		if err != nil || (n == 1 && buf[0] == '\n') {
			return
		}
	}
}

// clearClipboard 清剪貼簿；失敗要讓使用者知道 keyset 可能還在剪貼簿.
func (d *deps) clearClipboard() int {
	if err := d.Clip.Clear(); err != nil {
		return d.fail(exitFail, "clipboard clear failed: clear it manually")
	}
	return exitOK
}

// readLines 讀 stdin 全部非空行（去掉行尾 \r）.
func (d *deps) readLines() ([]string, error) {
	data, err := io.ReadAll(io.LimitReader(d.Stdin, maxStdinBytes+1))
	if err != nil {
		return nil, errors.New("cannot read stdin")
	}
	if len(data) > maxStdinBytes {
		return nil, errors.New("stdin too large")
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// parseEnvLine 拆 `NAME=value`；沒有 `=` 回 false.
func parseEnvLine(line string) (name, value string, ok bool) {
	name, value, ok = strings.Cut(line, "=")
	return strings.TrimSpace(name), strings.TrimSpace(value), ok
}

// ---- keyset new / rotate ----

// cmdKeysetNew [F34-41] 產生 keyset 放剪貼簿，等 Enter 後 Clear；keyset 不進 stdout/stderr.
func cmdKeysetNew(args []string, d *deps) int {
	fs := flag.NewFlagSet("keyset new", flag.ContinueOnError)
	fs.SetOutput(d.Stderr)
	n := fs.Int("keys", 5, "number of keys")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return exitUsage
	}
	if *n < 1 {
		return d.fail(exitUsage, "keyset new: --keys must be >= 1")
	}
	ks, err := crypto.GenerateKeySet(*n)
	if err != nil {
		return d.fail(exitFail, "keyset new: cannot generate keyset")
	}
	primary, err := crypto.PrimaryKeyID(ks)
	if err != nil {
		return d.fail(exitFail, "keyset new: cannot read generated keyset")
	}
	if err := d.Clip.Copy(ks); err != nil {
		return d.fail(exitFail, "keyset new: %v", err)
	}
	d.sayf("keyset copied (%d keys, primary id %s). %s", *n, idString(primary), clearHint)
	d.waitEnter() // [F34-41] Enter 之前剪貼簿保持原樣，使用者才有時間貼出去.
	return d.clearClipboard()
}

// cmdKeysetRotate [F34-42] 舊 keyset（剪貼簿）追加一把新主金鑰，結果放回剪貼簿.
func cmdKeysetRotate(args []string, d *deps) int {
	if len(args) > 0 {
		return d.fail(exitUsage, "%s", usageText)
	}
	ks, err := d.readKeyset()
	if err != nil {
		return d.fail(exitFail, "keyset rotate: %v", err)
	}
	before, err := crypto.KeyIDs(ks)
	if err != nil {
		return d.fail(exitFail, "keyset rotate: cannot read keyset")
	}
	rotated, err := crypto.RotateKeySet(ks)
	if err != nil {
		return d.fail(exitFail, "keyset rotate: cannot rotate keyset")
	}
	primary, err := crypto.PrimaryKeyID(rotated)
	if err != nil {
		return d.fail(exitFail, "keyset rotate: cannot read rotated keyset")
	}
	if err := d.Clip.Copy(rotated); err != nil {
		return d.fail(exitFail, "keyset rotate: %v", err)
	}
	d.sayf("rotated: %d -> %d keys, new primary id %s", len(before), len(before)+1, idString(primary))
	d.waitEnter()
	return d.clearClipboard()
}

// ---- encrypt ----

// cmdEncrypt [F34-42] 每欄以 SecretFields 的 Path 當 AAD；明文只經隱藏輸入，不回顯.
func cmdEncrypt(args []string, d *deps) int {
	if len(args) != 1 {
		return d.fail(exitUsage, "%s", usageText)
	}
	fields := config.SecretFields
	if args[0] != "--all" {
		sf, ok := fieldByEnv(args[0])
		if !ok {
			return d.fail(exitUsage, "encrypt: unknown env name")
		}
		fields = []config.SecretField{sf}
	}
	ks, err := d.readKeyset()
	if err != nil {
		return d.fail(exitFail, "encrypt: %v", err)
	}
	lines := make([]string, 0, len(fields))
	for _, sf := range fields {
		line, code := d.encryptField(ks, sf)
		if code != exitOK {
			return code
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	for _, line := range lines {
		d.sayf("%s", line)
	}
	return d.clearClipboard()
}

// encryptField 提示一欄並加密；回空字串代表略過.
// [F34-47] 明文只進 ReadSecret 與 EncryptValue，絕不寫到 stdout/stderr（即使 stdout 是終端機；只有 decrypt 例外）.
func (d *deps) encryptField(ks string, sf config.SecretField) (line string, code int) {
	secret, err := d.Prompt.ReadSecret(sf.Env + " (empty = skip):")
	if err != nil {
		return "", d.fail(exitFail, "encrypt %s: cannot read input", sf.Env)
	}
	defer clear(secret)
	if len(secret) >= maxSecretBytes {
		return "", d.fail(exitFail, "encrypt %s: input too long (>= %d bytes), not truncated", sf.Env, maxSecretBytes)
	}
	if len(secret) == 0 {
		return "", exitOK
	}
	enc, err := crypto.EncryptValue(string(secret), ks, sf.Path)
	if err != nil {
		return "", d.fail(exitFail, "encrypt %s: %s", sf.Env, reasonOf(err))
	}
	return sf.Env + "=" + enc, exitOK
}

// ---- reencrypt ----

// cmdReencrypt [F34-43] NeedsReencrypt 為 true 才重加密、其餘逐位元組原樣；all-or-nothing.
func cmdReencrypt(args []string, d *deps) int {
	if len(args) > 0 {
		return d.fail(exitUsage, "%s", usageText)
	}
	ks, err := d.readKeyset()
	if err != nil {
		return d.fail(exitFail, "reencrypt: %v", err)
	}
	input, err := d.readLines()
	if err != nil {
		return d.fail(exitFail, "reencrypt: %v", err)
	}
	if len(input) == 0 {
		return d.fail(exitFail, "reencrypt: no input lines")
	}
	out := make([]string, 0, len(input))
	processed := 0
	for i, line := range input {
		res, changed, code := d.reencryptLine(ks, i+1, line)
		if code != exitOK {
			return code
		}
		if changed {
			processed++
		}
		out = append(out, res)
	}
	for _, line := range out {
		d.sayf("%s", line)
	}
	if _, err := fmt.Fprintf(d.Stderr, "reencrypt: processed=%d skipped=%d\n", processed, len(out)-processed); err != nil {
		return exitFail
	}
	return d.clearClipboard()
}

// reencryptLine 處理一行；每個非空值都先實解（驗證金鑰與 AAD），再依 NeedsReencrypt 決定重加密或原樣.
func (d *deps) reencryptLine(ks string, lineNo int, line string) (res string, changed bool, code int) {
	name, value, ok := parseEnvLine(line)
	if !ok {
		return "", false, d.fail(exitFail, "reencrypt: line %d: bad format", lineNo)
	}
	sf, known := fieldByEnv(name)
	if !known {
		return "", false, d.fail(exitFail, "reencrypt: line %d: unknown env", lineNo)
	}
	if value == "" {
		return line, false, exitOK
	}
	plain, _, err := crypto.DecryptValue(value, ks, sf.Path)
	if err != nil {
		return "", false, d.fail(exitFail, "reencrypt %s: %s", sf.Env, reasonOf(err))
	}
	needs, err := crypto.NeedsReencrypt(value, ks)
	if err != nil {
		return "", false, d.fail(exitFail, "reencrypt %s: %s", sf.Env, reasonOf(err))
	}
	if !needs {
		return line, false, exitOK
	}
	enc, err := crypto.EncryptValue(plain, ks, sf.Path)
	if err != nil {
		return "", false, d.fail(exitFail, "reencrypt %s: %s", sf.Env, reasonOf(err))
	}
	if check, _, err := crypto.DecryptValue(enc, ks, sf.Path); err != nil || check != plain {
		return "", false, d.fail(exitFail, "reencrypt %s: authentication failed", sf.Env)
	}
	return sf.Env + "=" + enc, true, exitOK
}

// ---- decrypt ----

// cmdDecrypt [F34-46] 唯一會把明文印到 stdout 的子命令；終端機檢查先於讀剪貼簿、讀 stdin、解密.
func cmdDecrypt(args []string, d *deps) int {
	if len(args) != 1 {
		return d.fail(exitUsage, "%s", usageText)
	}
	sf, ok := fieldByEnv(args[0])
	if !ok {
		return d.fail(exitUsage, "decrypt: unknown env name")
	}
	if !d.IsTerminal() {
		return d.fail(exitFail, "decrypt %s: need a terminal", sf.Env)
	}
	raw, err := d.Clip.Read()
	ks := strings.TrimSpace(raw)
	if err != nil || ks == "" {
		return d.fail(exitFail, "decrypt %s: no keyset", sf.Env)
	}
	input, err := d.readLines()
	if err != nil || len(input) != 1 {
		return d.fail(exitFail, "decrypt %s: bad format", sf.Env)
	}
	enc := input[0]
	if !crypto.IsENC(strings.TrimSpace(enc)) {
		name, value, found := parseEnvLine(enc)
		if !found || name != sf.Env {
			return d.fail(exitFail, "decrypt %s: bad format", sf.Env)
		}
		enc = value
	}
	plain, _, err := d.decrypt(strings.TrimSpace(enc), ks, sf.Path)
	if err != nil {
		return d.fail(exitFail, "decrypt %s: %s", sf.Env, reasonOf(err))
	}
	d.sayf("%s", plain)
	return d.clearClipboard()
}

// ---- status / prune 共用：stdin 的 `NAME=ENC(...)` 或 `NAME=` ----

// parseSecretInput 把 stdin 行解成 env → 值（空字串 = 明示未設）；未知名、重複、缺 `=` 回 exitUsage，非 ENC 的非空值回 exitFail.
func (d *deps) parseSecretInput(lines []string) (values map[string]string, code int) {
	values = make(map[string]string, len(config.SecretFields))
	for i, line := range lines {
		name, value, ok := parseEnvLine(line)
		if !ok {
			return nil, d.fail(exitUsage, "line %d: bad format", i+1)
		}
		sf, known := fieldByEnv(name)
		if !known {
			return nil, d.fail(exitUsage, "line %d: unknown env", i+1)
		}
		if _, dup := values[sf.Env]; dup {
			return nil, d.fail(exitUsage, "%s: duplicate line", sf.Env)
		}
		if value != "" && !crypto.IsENC(value) {
			return nil, d.fail(exitFail, "%s: bad format (not an ENC value)", sf.Env)
		}
		values[sf.Env] = value
	}
	return values, exitOK
}

// ---- keyset status ----

// cmdKeysetStatus [F34-44] 每個非空行都實解（不只看前綴）；任一行解不開 → exit 1、stdout 空.
func cmdKeysetStatus(args []string, d *deps) int {
	if len(args) > 0 {
		return d.fail(exitUsage, "%s", usageText)
	}
	ks, err := d.readKeyset()
	if err != nil {
		return d.fail(exitFail, "keyset status: %v", err)
	}
	infos, err := crypto.KeyIDs(ks)
	if err != nil {
		return d.fail(exitFail, "keyset status: cannot read keyset")
	}
	input, err := d.readLines()
	if err != nil {
		return d.fail(exitFail, "keyset status: %v", err)
	}
	values, code := d.parseSecretInput(input)
	if code != exitOK {
		return code
	}
	used := make(map[uint32]int, len(infos))
	rows := make([]string, 0, len(values))
	for _, sf := range config.SecretFields {
		value, present := values[sf.Env]
		if !present {
			continue
		}
		if value == "" {
			rows = append(rows, sf.Env+" unset")
			continue
		}
		id, err := d.verifiedKeyID(ks, sf, value)
		if err != nil {
			return d.fail(exitFail, "keyset status: %s: %s", sf.Env, reasonOf(err))
		}
		used[id]++
		rows = append(rows, sf.Env+" key="+idString(id))
	}
	for _, info := range infos {
		d.sayf("key %s primary=%t used=%d", idString(info.ID), info.Primary, used[info.ID])
	}
	for _, row := range rows {
		d.sayf("%s", row)
	}
	return d.clearClipboard()
}

// verifiedKeyID 先實解（驗金鑰與 AAD），再取密文前綴的 key id.
func (*deps) verifiedKeyID(ks string, sf config.SecretField, enc string) (uint32, error) {
	if _, _, err := crypto.DecryptValue(enc, ks, sf.Path); err != nil { // [F34-44] MUST 實解，不能只信前綴.
		return 0, err
	}
	return crypto.CiphertextKeyID(enc)
}

// ---- keyset prune ----

// pruneTargets 決定要移除哪些 key id：--key 指定一把，否則所有非主金鑰.
func pruneTargets(infos []crypto.KeyInfo, keyFlag string) ([]uint32, error) {
	if keyFlag != "" {
		id, err := strconv.ParseUint(keyFlag, 10, 32)
		if err != nil {
			return nil, errors.New("--key must be a decimal key id")
		}
		return []uint32{uint32(id)}, nil
	}
	var ids []uint32
	for _, info := range infos {
		if !info.Primary {
			ids = append(ids, info.ID)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("no non-primary keys to remove")
	}
	return ids, nil
}

// cmdKeysetPrune [F34-45] 移除舊金鑰前先證明 11 行全部不依賴它們、且 K3 實解每一行；任何一項不過 → 剪貼簿不動、stdout 空.
func cmdKeysetPrune(args []string, d *deps) int {
	fs := flag.NewFlagSet("keyset prune", flag.ContinueOnError)
	fs.SetOutput(d.Stderr)
	keyFlag := fs.String("key", "", "key id to remove (default: every non-primary key)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return exitUsage
	}
	ks, err := d.readKeyset()
	if err != nil {
		return d.fail(exitFail, "keyset prune: %v", err)
	}
	infos, err := crypto.KeyIDs(ks)
	if err != nil {
		return d.fail(exitFail, "keyset prune: cannot read keyset")
	}
	remove, err := pruneTargets(infos, *keyFlag)
	if err != nil {
		return d.fail(exitUsage, "keyset prune: %v", err)
	}
	values, code := d.pruneInput()
	if code != exitOK {
		return code
	}
	pruned, err := crypto.PruneKeySet(ks, remove)
	if err != nil { // [F34-45] 不存在的 id 或 primary 由 PruneKeySet 拒絕，cmd 必須照單全收.
		return d.fail(exitFail, "keyset prune: cannot remove key %s (missing or primary)", joinIDs(remove))
	}
	if code := d.pruneCheck(pruned, values, remove); code != exitOK {
		return code
	}
	if err := d.Clip.Copy(pruned); err != nil {
		return d.fail(exitFail, "keyset prune: %v", err)
	}
	d.sayf("pruned: %d -> %d keys, removed %s. %s", len(infos), len(infos)-len(remove), joinIDs(remove), clearHint)
	d.waitEnter()
	return d.clearClipboard()
}

// pruneInput 讀 stdin 並要求 SecretFields 的 11 個 env 各出現一次（缺名 → exit 2）.
func (d *deps) pruneInput() (values map[string]string, code int) {
	input, err := d.readLines()
	if err != nil {
		return nil, d.fail(exitFail, "keyset prune: %v", err)
	}
	values, code = d.parseSecretInput(input)
	if code != exitOK {
		return nil, code
	}
	var missing []string
	for _, sf := range config.SecretFields {
		if _, ok := values[sf.Env]; !ok {
			missing = append(missing, sf.Env)
		}
	}
	if len(missing) > 0 {
		return nil, d.fail(exitUsage, "keyset prune: missing lines for: %s", strings.Join(missing, ","))
	}
	return values, exitOK
}

// pruneCheck 先看密文前綴有無依賴被移除的 key，再以 K3 實解每個非空行.
func (d *deps) pruneCheck(pruned string, values map[string]string, remove []uint32) int {
	removed := make(map[uint32]bool, len(remove))
	for _, id := range remove {
		removed[id] = true
	}
	blocked := false
	for _, sf := range config.SecretFields {
		value := values[sf.Env]
		if value == "" {
			continue
		}
		if id, err := crypto.CiphertextKeyID(value); err == nil && removed[id] {
			blocked = true
			d.fail(exitFail, "keyset prune: refused: %s still uses key %s", sf.Env, idString(id))
		}
	}
	if blocked {
		return exitFail
	}
	for _, sf := range config.SecretFields {
		value := values[sf.Env]
		if value == "" {
			continue
		}
		if _, _, err := crypto.DecryptValue(value, pruned, sf.Path); err != nil { // [F34-45] K3 MUST 實解每一行.
			return d.fail(exitFail, "keyset prune: %s: %s", sf.Env, reasonOf(err))
		}
	}
	return exitOK
}

func joinIDs(ids []uint32) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = idString(id)
	}
	return strings.Join(parts, ",")
}
