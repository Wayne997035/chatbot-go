// Command seal 管理設定檔加密用的 AES-256-GCM 金鑰集（含金鑰輪替），並把設定值加密成 ENC(...).
//
// 只在本機執行、不連網. 明文只從標準輸入讀（例如 pbpaste | ...），不會出現在指令或歷史紀錄裡；
// 金鑰集預設存在 ~/.config/chatbot-go/crypto.keyset（權限 600，在專案資料夾外，不會被 commit）.
//
//	go run ./cmd/seal keyset new             產生新的金鑰集
//	go run ./cmd/seal keyset info            列出金鑰 ID、哪一把是主要金鑰（不印金鑰本身）
//	go run ./cmd/seal keyset rotate          加入新金鑰並設為主要；舊金鑰保留，舊的 ENC 值照樣解得開
//	go run ./cmd/seal keyset prune           刪除舊金鑰（所有值都重新加密之後才做）
//	pbpaste | go run ./cmd/seal encrypt      明文 → ENC(...)，一行一個
//	pbpaste | go run ./cmd/seal reencrypt    舊的 ENC(...) → 用主要金鑰重新加密
//	pbpaste | go run ./cmd/seal check        檢查 ENC(...) 解不解得開、用哪一把金鑰（不印明文）
//
// 每個指令都可以加 -keyset <路徑> 改用其他金鑰集檔案.
package main

import (
	"bufio"
	"chatbot-go/internal/crypto"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "找不到使用者目錄：", err)
		os.Exit(1)
	}
	env := &env{
		stdin: os.Stdin, stdout: os.Stdout, home: home,
		stdinIsTerminal: func() bool {
			fi, err := os.Stdin.Stat()
			return err == nil && fi.Mode()&os.ModeCharDevice != 0
		},
	}
	if err := run(os.Args[1:], env); err != nil {
		fmt.Fprintln(os.Stderr, "錯誤：", err)
		os.Exit(1)
	}
}

// env 指令的輸入輸出（測試時替換）.
type env struct {
	stdin           io.Reader
	stdout          io.Writer
	home            string
	stdinIsTerminal func() bool
}

const usage = `用法：
  go run ./cmd/seal keyset new | info | rotate | prune
  pbpaste | go run ./cmd/seal encrypt | reencrypt | check
加 -keyset <路徑> 可改用其他金鑰集檔案（預設 ~/.config/chatbot-go/crypto.keyset）`

func run(args []string, e *env) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	cmd, rest := args[0], args[1:]
	if cmd == "keyset" {
		if len(rest) == 0 {
			return errors.New(usage)
		}
		cmd, rest = "keyset "+rest[0], rest[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("keyset", filepath.Join(e.home, ".config", "chatbot-go", "crypto.keyset"), "金鑰集檔案")
	if err := fs.Parse(rest); err != nil {
		return fmt.Errorf("%w\n%s", err, usage)
	}

	switch cmd {
	case "keyset new":
		return keysetNew(e, *path)
	case "keyset info":
		return keysetInfo(e, *path)
	case "keyset rotate":
		return keysetRotate(e, *path)
	case "keyset prune":
		return keysetPrune(e, *path)
	case "encrypt", "reencrypt", "check":
		return transform(e, cmd, *path)
	default:
		return errors.New(usage)
	}
}

func keysetNew(e *env, path string) error {
	ks, err := crypto.NewKeySet()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // 本機工具：路徑是使用者自己用 -keyset 指定的
		return err
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s 已經有金鑰集了；要換金鑰請用 keyset rotate，或用 -keyset 指定別的路徑", path)
	}
	if err != nil {
		return err
	}
	if _, err := f.WriteString(ks + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	primary, _ := crypto.PrimaryKeyID(ks)
	_, _ = fmt.Fprintf(e.stdout, "已產生新的金鑰集（AES-256-GCM，主要金鑰 %d）：%s\n"+
		"下一步：執行 pbcopy < %s，把它貼到部署環境的 CRYPTO_KEYSET\n", primary, path, path)
	return nil
}

func keysetInfo(e *env, path string) error {
	ks, err := readKeySet(path)
	if err != nil {
		return err
	}
	keys, err := crypto.DescribeKeySet(ks)
	if err != nil {
		return err
	}
	for _, k := range keys {
		role, status := "舊金鑰", "停用"
		if k.Primary {
			role = "主要"
		}
		if k.Enabled {
			status = "啟用"
		}
		_, _ = fmt.Fprintf(e.stdout, "金鑰 %d\t%s\t%s\tAES-256-GCM\n", k.ID, role, status)
	}
	return nil
}

func keysetRotate(e *env, path string) error {
	ks, err := readKeySet(path)
	if err != nil {
		return err
	}
	rotated, primary, err := crypto.RotateKeySet(ks)
	if err != nil {
		return err
	}
	if err := writeKeySet(path, rotated); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(e.stdout, "已加入新的主要金鑰 %d（舊金鑰保留，現有的 ENC 值照樣解得開）。\n"+
		"下一步：\n"+
		"  1. pbcopy < %s，更新部署環境的 CRYPTO_KEYSET 並重新部署\n"+
		"  2. 把每個 ENC(...) 值複製後執行 pbpaste | go run ./cmd/seal reencrypt，換成新的值\n"+
		"  3. 服務的 log 不再出現 \"older key\" 警告後，才執行 keyset prune 刪除舊金鑰\n", primary, path)
	return nil
}

func keysetPrune(e *env, path string) error {
	ks, err := readKeySet(path)
	if err != nil {
		return err
	}
	pruned, removed, err := crypto.PruneKeySet(ks)
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		_, _ = fmt.Fprintln(e.stdout, "只有主要金鑰，沒有可刪除的舊金鑰。")
		return nil
	}
	if err := writeKeySet(path, pruned); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(e.stdout, "已刪除舊金鑰 %v。用這些金鑰加密、還沒重新加密的值從此解不開。\n"+
		"下一步：pbcopy < %s，更新部署環境的 CRYPTO_KEYSET 並重新部署\n", removed, path)
	return nil
}

// transform 從標準輸入一行一個值處理：encrypt（明文 → ENC）、reencrypt（ENC → 主要金鑰的 ENC）、check.
func transform(e *env, cmd, path string) error {
	if e.stdinIsTerminal() {
		return fmt.Errorf("請用管線輸入，避免值顯示在畫面或留在歷史紀錄：先複製值，再執行 pbpaste | go run ./cmd/seal %s", cmd)
	}
	ks, err := readKeySet(path)
	if err != nil {
		return err
	}
	primary, err := crypto.PrimaryKeyID(ks)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(e.stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line, failed := 0, 0
	for sc.Scan() {
		line++
		in := strings.TrimSpace(sc.Text())
		if in == "" {
			continue
		}
		out, err := transformOne(cmd, in, ks, primary)
		if err != nil {
			failed++
			_, _ = fmt.Fprintf(e.stdout, "第 %d 行：%v\n", line, err)
			continue
		}
		if cmd == "check" {
			out = fmt.Sprintf("第 %d 行：%s", line, out)
		}
		_, _ = fmt.Fprintln(e.stdout, out)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if line == 0 {
		return errors.New("沒有讀到任何值；請先複製要處理的值")
	}
	if failed > 0 {
		return fmt.Errorf("%d 行處理失敗", failed)
	}
	return nil
}

func transformOne(cmd, in, ks string, primary uint32) (string, error) {
	if cmd == "encrypt" {
		if strings.HasPrefix(in, "ENC(") {
			return "", errors.New("這已經是加密過的值；要換成新金鑰請用 reencrypt")
		}
		return seal(in, ks)
	}
	ct, ok := strings.CutPrefix(in, "ENC(")
	ct, ok2 := strings.CutSuffix(ct, ")")
	if !ok || !ok2 {
		return "", errors.New("不是 ENC(...) 格式")
	}
	plain, err := crypto.Decrypt(ct, ks)
	if err != nil {
		return "", errors.New("解不開（金鑰不對，或加密它的金鑰已經刪除）")
	}
	if cmd == "reencrypt" {
		return seal(plain, ks)
	}
	if id, _ := crypto.KeyIDOf(ct); id != primary {
		return fmt.Sprintf("可以解密，但用的是舊金鑰 %d，請用 reencrypt 重新加密", id), nil
	}
	return fmt.Sprintf("可以解密，用的是主要金鑰 %d", primary), nil
}

func seal(plain, ks string) (string, error) {
	ct, err := crypto.Encrypt(plain, ks)
	if err != nil {
		return "", err
	}
	return "ENC(" + ct + ")", nil
}

func readKeySet(path string) (string, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("找不到金鑰集 %s；第一次使用請先執行 go run ./cmd/seal keyset new", path)
	}
	if err != nil {
		return "", err
	}
	ks := strings.TrimSpace(string(data))
	if err := crypto.ValidateKeySet(ks); err != nil {
		return "", fmt.Errorf("金鑰集 %s 無法使用：%w", path, err)
	}
	return ks, nil
}

// writeKeySet 先寫到同目錄的暫存檔（權限 600）再改名，寫到一半中斷也不會弄壞原本的金鑰集.
func writeKeySet(path, ks string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".crypto.keyset-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() //nolint:gosec // 暫存檔是這裡自己建立的
	if _, err := tmp.WriteString(ks + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path) //nolint:gosec // 本機工具：路徑是使用者自己用 -keyset 指定的
}
