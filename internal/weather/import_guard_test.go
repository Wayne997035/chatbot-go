package weather

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const aiImportPath = "chatbot-go/internal/ai"

// aiImportViolations 解析 dir 內全部非測試 .go 的 import，回傳違規（檔名: import 路徑）與解析檔數.
// 解析不到任何檔、全部檔零 import、或任一檔解析失敗都回 error，守門不會空轉變綠.
func aiImportViolations(dir string) (violations []string, parsed int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	imports := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.ImportsOnly)
		if perr != nil {
			return nil, parsed, fmt.Errorf("parse %s: %w", name, perr)
		}
		parsed++
		for _, imp := range f.Imports {
			imports++
			path, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				return nil, parsed, fmt.Errorf("unquote import in %s: %w", name, uerr)
			}
			if path == aiImportPath || strings.HasPrefix(path, aiImportPath+"/") {
				violations = append(violations, name+": "+path)
			}
		}
	}
	if parsed == 0 {
		return nil, 0, fmt.Errorf("parsed no non-test .go files in %q: the guard would pass vacuously", dir)
	}
	if imports == 0 {
		return nil, parsed, fmt.Errorf("parsed no imports in %d files: the guard would pass vacuously", parsed)
	}
	return violations, parsed, nil
}

// [F37-03] 守門掃整個 internal/weather 的非測試檔（原本只讀 advice.go）.
func TestWeatherPackageDoesNotImportAI(t *testing.T) {
	violations, parsed, err := aiImportViolations(".")
	if err != nil {
		t.Fatal(err)
	}
	if parsed < 8 {
		t.Fatalf("parsed only %d files, want >= 8: the guard is not covering the package", parsed)
	}
	for _, v := range violations {
		t.Errorf("%s; internal/weather must stay free of the AI package", v)
	}
}

// [F37-03] 守門自己的正對照：違規要紅、兄弟名稱不誤判、空目錄/零 import/壞檔要報錯.
func TestAIImportGuardHelper(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		wantViol  string
		wantError string
	}{
		{"direct ai import in any file", map[string]string{"x.go": "package w\nimport _ \"chatbot-go/internal/ai\"\n"}, "x.go", ""},
		{"sibling package name is not ai", map[string]string{"y.go": "package w\nimport _ \"chatbot-go/internal/aibudget\"\n"}, "", ""},
		{"subpackage of ai", map[string]string{"z.go": "package w\nimport _ \"chatbot-go/internal/ai/sub\"\n"}, "z.go", ""},
		{"test file only", map[string]string{"a_test.go": "package w\nimport _ \"chatbot-go/internal/ai\"\n"}, "", "no non-test"},
		{"empty dir", map[string]string{}, "", "no non-test"},
		{"files without imports", map[string]string{"n.go": "package w\n"}, "", "no imports"},
		{"syntax error", map[string]string{"b.go": "package w\nimport (\n"}, "", "parse b.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for n, body := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			viol, _, err := aiImportViolations(dir)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantViol == "" && len(viol) != 0 {
				t.Fatalf("unexpected violations %v", viol)
			}
			if tc.wantViol != "" && (len(viol) != 1 || !strings.HasPrefix(viol[0], tc.wantViol)) {
				t.Fatalf("violations = %v, want one naming %s", viol, tc.wantViol)
			}
		})
	}
}
