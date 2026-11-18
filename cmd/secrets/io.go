package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"
)

// errClipboardUnavailable 代表找不到 pbcopy/pbpaste（非 macOS 或不在 PATH）.
var errClipboardUnavailable = errors.New("clipboard unavailable")

// pbClipboard [F34-41] macOS 剪貼簿；命令名寫死，不由輸入決定.
type pbClipboard struct{}

func (pbClipboard) Read() (string, error) {
	if _, err := exec.LookPath("pbpaste"); err != nil {
		return "", errClipboardUnavailable
	}
	out, err := exec.CommandContext(context.Background(), "pbpaste").Output()
	if err != nil {
		return "", errClipboardUnavailable
	}
	return string(out), nil
}

func (pbClipboard) Copy(text string) error {
	if _, err := exec.LookPath("pbcopy"); err != nil {
		return errClipboardUnavailable
	}
	cmd := exec.CommandContext(context.Background(), "pbcopy")
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return errClipboardUnavailable
	}
	return nil
}

func (c pbClipboard) Clear() error { return c.Copy("") }

// termPrompter [F34-41] 以 golang.org/x/term 做不回顯輸入；提示字只寫 stderr.
type termPrompter struct{}

func (termPrompter) ReadSecret(label string) ([]byte, error) {
	if _, err := fmt.Fprint(os.Stderr, label+" "); err != nil {
		return nil, err
	}
	secret, err := term.ReadPassword(int(os.Stdin.Fd())) //nolint:gosec // G115: 檔案描述符遠小於 int 上限，不會溢位
	if _, werr := fmt.Fprintln(os.Stderr); werr != nil && err == nil {
		err = werr
	}
	return secret, err
}

// productionDeps 組出真實的依賴；IsTerminal 看 stdout（decrypt 的輸出去向）.
func productionDeps() deps {
	return deps{
		Clip:   pbClipboard{},
		Prompt: termPrompter{},
		IsTerminal: func() bool {
			return term.IsTerminal(int(os.Stdout.Fd())) //nolint:gosec // G115: 檔案描述符遠小於 int 上限，不會溢位
		},
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}
