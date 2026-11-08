package logging

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoContextlessLogCalls enforces LG-2: task-path code must log through
// the *Context slog variants (so contextHandler can attach task_id/round/
// step_id/invocation_id) and must not use the stdlib log package. Startup
// code in main.go/config.go is exempt.
func TestNoContextlessLogCalls(t *testing.T) {
	roots := []string{
		"../control-plane", "../../cmd/control-plane",
		"../../cmd/container-launcher", "../launcher",
	}
	bad := map[string]bool{"Debug": true, "Info": true, "Warn": true, "Error": true}
	fset := token.NewFileSet()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			startup := filepath.Base(path) == "main.go" || filepath.Base(path) == "config.go"
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				pos := fset.Position(call.Pos())
				switch {
				case id.Name == "slog" && !startup && bad[sel.Sel.Name]:
					t.Errorf("%s: slog.%s without context; use slog.%sContext", pos, sel.Sel.Name, sel.Sel.Name)
				case id.Name == "log" && !startup && strings.HasPrefix(sel.Sel.Name, "Print"),
					id.Name == "log" && !startup && strings.HasPrefix(sel.Sel.Name, "Fatal"):
					t.Errorf("%s: stdlib log.%s outside startup code", pos, sel.Sel.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
