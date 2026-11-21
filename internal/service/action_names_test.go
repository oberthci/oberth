package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// The two actions share a class today; retain their distinct lookup identities.
func TestWaitHandlersUseTheirOwnAuthorityRows(t *testing.T) {
	t.Parallel()
	f, err := parser.ParseFile(token.NewFileSet(), "handlers.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for method, want := range map[string]string{"PrepareWait": "wait", "PrepareRunWait": "run_wait"} {
		got := handlerAction(f, method)
		if got != want {
			t.Fatalf("%s authority row %q, want %q", method, got, want)
		}
	}
}

func handlerAction(file *ast.File, method string) string {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == method {
			return actionField(fn)
		}
	}
	return ""
}
func actionField(node ast.Node) string {
	got := ""
	ast.Inspect(node, func(n ast.Node) bool {
		pair, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := pair.Key.(*ast.Ident)
		if !ok || key.Name != "Action" {
			return true
		}
		if value, ok := pair.Value.(*ast.BasicLit); ok {
			got, _ = strconv.Unquote(value.Value)
		}
		return true
	})
	return got
}
