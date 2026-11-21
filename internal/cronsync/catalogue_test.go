package cronsync

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestDisplayCataloguesContainOnlyCompleteLiterals(t *testing.T) {
	for _, catalogue := range []struct{ file, function string }{{"errors.go", "messages"}, {"../schedule/errors.go", "reasons"}} {
		checkCatalogueFile(t, catalogue.file, catalogue.function)
	}
}
func checkCatalogueFile(t *testing.T, path, name string) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != name {
			continue
		}
		for _, item := range catalogueItems(t, function) {
			checkLiteralMessage(t, item)
		}
		return
	}
	t.Fatal("missing catalogue", path, name)
}
func catalogueItems(t *testing.T, function *ast.FuncDecl) []ast.Expr {
	t.Helper()
	if len(function.Body.List) != 1 {
		t.Fatal("catalogue must return only literals")
	}
	returned, ok := function.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(returned.Results) != 1 {
		t.Fatal("missing literal catalogue")
	}
	list, ok := returned.Results[0].(*ast.CompositeLit)
	if !ok {
		t.Fatal("catalogue is not literal")
	}
	return list.Elts
}
func checkLiteralMessage(t *testing.T, expression ast.Expr) {
	t.Helper()
	if row, ok := expression.(*ast.CompositeLit); ok {
		if len(row.Elts) != 3 {
			t.Fatal("message row needs code, reason and remedy")
		}
		literalMessage(t, row.Elts[1], 40)
		literalMessage(t, row.Elts[2], 120)
		return
	}
	literalMessage(t, expression, 80)
}
func literalMessage(t *testing.T, expression ast.Expr, limit int) {
	t.Helper()
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		t.Fatal("generated message is not a string literal")
	}
	text, err := strconv.Unquote(literal.Value)
	if err != nil {
		t.Fatal(err)
	}
	if err := publicerr.CheckMessage(text, limit); err != nil {
		t.Fatal(text, err)
	}
}
