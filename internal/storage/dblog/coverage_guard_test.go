package dblog

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var writeMethods = map[string]bool{
	"InsertOne": true, "InsertMany": true, "UpdateOne": true, "UpdateMany": true, "ReplaceOne": true,
	"FindOneAndUpdate": true, "DeleteOne": true, "DeleteMany": true, "BulkWrite": true,
}

// exemptWrite [F37-08] 唯一免經 dblog 的寫入：forecast 走批次行（另由 checkBatchLine 守住）.
func exemptWrite(file, fn string) bool {
	return filepath.Base(file) == "weather_store.go" && fn == "Upsert"
}

// checkWriteCoverage 解析 dir 下全部非測試 .go：每個函式內 dblog.Write 呼叫數必須 >= 寫入呼叫點數.
// 回傳解析檔數、寫入呼叫點總數與違規清單；檔數或寫入點為 0 由呼叫端視為紅（不是綠）.
func checkWriteCoverage(dir string) (files, writes int, problems []string, err error) {
	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		files++
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			nWrite, nLog := 0, 0
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if id, isID := sel.X.(*ast.Ident); isID && id.Name == "dblog" && sel.Sel.Name == "Write" {
					nLog++
					return true
				}
				if writeMethods[sel.Sel.Name] {
					if id, isID := sel.X.(*ast.Ident); isID && id.Name == "options" {
						return true // options.UpdateOne() 等選項建構子不是寫入
					}
					nWrite++
				}
				return true
			})
			writes += nWrite
			if nWrite > 0 && nLog < nWrite && !exemptWrite(path, fd.Name.Name) {
				problems = append(problems, fmt.Sprintf("%s: %s has %d write call(s) but %d dblog.Write", filepath.Base(path), fd.Name.Name, nWrite, nLog))
			}
		}
		return nil
	})
	return files, writes, problems, walkErr
}

// checkBatchLine api_client.go 的 fetchAndProcess 內必須有 slog.Info，且參數含 "total"、"ok"、"failed" 三個 key.
func checkBatchLine(path string) error {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return err
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "fetchAndProcess" || fd.Body == nil {
			continue
		}
		found := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Info" {
				return true
			}
			if id, isID := sel.X.(*ast.Ident); !isID || id.Name != "slog" {
				return true
			}
			keys := map[string]bool{}
			for _, a := range call.Args {
				if lit, isLit := a.(*ast.BasicLit); isLit && lit.Kind == token.STRING {
					if s, uerr := strconv.Unquote(lit.Value); uerr == nil {
						keys[s] = true
					}
				}
			}
			if keys["total"] && keys["ok"] && keys["failed"] {
				found = true
			}
			return true
		})
		if !found {
			return fmt.Errorf("fetchAndProcess has no slog.Info with keys total, ok, failed")
		}
		return nil
	}
	return fmt.Errorf("fetchAndProcess not found in %s", path)
}

// [F37-08] 真實碼：internal/storage/database 的每個寫入點都經 dblog.Write，forecast 有批次行.
func TestEveryMongoWriteIsLogged(t *testing.T) {
	files, writes, problems, err := checkWriteCoverage(filepath.Join("..", "database"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if files == 0 {
		t.Fatal("parsed no non-test .go files; guard would pass vacuously")
	}
	if writes == 0 {
		t.Fatal("found 0 write call sites; guard would pass vacuously")
	}
	for _, p := range problems {
		t.Error(p)
	}
	if err := checkBatchLine(filepath.Join("..", "..", "weather", "api_client.go")); err != nil {
		t.Error(err)
	}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// [F37-08] 守門自己的正對照：好的綠、各種缺漏紅、空目錄紅（不是綠）.
func TestCoverageGuardCatchesViolations(t *testing.T) {
	const head = "package x\nimport (\"context\"; \"go.mongodb.org/mongo-driver/v2/mongo/options\")\nvar _ = options.UpdateOne\n"
	tests := []struct {
		name         string
		files        map[string]string
		wantFiles    int
		wantWrites   int
		wantProblems int
	}{
		{"logged write", map[string]string{"a.go": head + "func (r *R) F(ctx context.Context) { _, err := r.c.InsertOne(ctx, 1); dblog.Write(ctx, \"a\", \"insert\", err) }"}, 1, 1, 0},
		{"missing helper", map[string]string{"a.go": head + "func (r *R) F(ctx context.Context) { r.c.InsertOne(ctx, 1) }"}, 1, 1, 1},
		{"second write bypasses helper", map[string]string{"a.go": head + "func (r *R) F(ctx context.Context) { _, err := r.c.InsertOne(ctx, 1); dblog.Write(ctx, \"a\", \"insert\", err); r.c.DeleteOne(ctx, 2) }"}, 1, 2, 1},
		{"new unlogged function", map[string]string{"a.go": head + "func (r *R) G(ctx context.Context) { r.c.BulkWrite(ctx, nil) }\nfunc (r *R) F(ctx context.Context) { _, err := r.c.InsertOne(ctx, 1); dblog.Write(ctx, \"a\", \"insert\", err) }"}, 1, 2, 1},
		{"options constructor is not a write", map[string]string{"a.go": head + "func F() { _ = options.UpdateOne().SetUpsert(true) }"}, 1, 0, 0},
		{"weather Upsert exempt", map[string]string{"weather_store.go": head + "func (r *R) Upsert(ctx context.Context) error { _, err := r.c.UpdateOne(ctx, 1, 2); return err }"}, 1, 1, 0},
		{"other Upsert not exempt", map[string]string{"user_store.go": head + "func (r *R) Upsert(ctx context.Context) error { _, err := r.c.UpdateOne(ctx, 1, 2); return err }"}, 1, 1, 1},
		{"empty dir", map[string]string{}, 0, 0, 0},
		{"test files ignored", map[string]string{"a_test.go": head + "func F() { r.c.InsertOne(1) }"}, 0, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files, writes, problems, err := checkWriteCoverage(writeTree(t, tc.files))
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if files != tc.wantFiles || writes != tc.wantWrites || len(problems) != tc.wantProblems {
				t.Errorf("files=%d writes=%d problems=%v; want %d/%d/%d", files, writes, problems, tc.wantFiles, tc.wantWrites, tc.wantProblems)
			}
		})
	}
}

// [F37-08] 批次行守門：有三個 key 才綠；少一個、不在 fetchAndProcess 內、函式不存在都紅.
func TestBatchLineGuardCatchesViolations(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr bool
	}{
		{"ok", `package w; import "log/slog"; func fetchAndProcess() { slog.Info("m", "total", 1, "ok", 1, "failed", 0) }`, false},
		{"missing failed", `package w; import "log/slog"; func fetchAndProcess() { slog.Info("m", "total", 1, "ok", 1) }`, true},
		{"debug not info", `package w; import "log/slog"; func fetchAndProcess() { slog.Debug("m", "total", 1, "ok", 1, "failed", 0) }`, true},
		{"line in another function", `package w; import "log/slog"; func other() { slog.Info("m", "total", 1, "ok", 1, "failed", 0) }; func fetchAndProcess() {}`, true},
		{"function missing", `package w; func other() {}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTree(t, map[string]string{"api_client.go": tc.src})
			err := checkBatchLine(filepath.Join(dir, "api_client.go"))
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
