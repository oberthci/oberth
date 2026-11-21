package sourcecontract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

// The invalid factory is the single source-refusal catalogue. Walk its literal
// call sites so adding a reason requires no duplicate list of message text.
func TestEverySourceRefusalFitsMessageLimits(t *testing.T) {
	for text, limit := range map[string]int{(&InvalidError{}).Reason(): 80, (&InvalidError{}).Remedy(): 120} {
		if err := publicerr.CheckMessage(text, limit); err != nil {
			t.Fatal(err, text)
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		node, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(node, func(n ast.Node) bool { return checkSourceRefusal(t, n) })
	}
}

func catalogueLiteral(t *testing.T, expr ast.Expr) string {
	t.Helper()
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		t.Fatal("source refusal must use a catalogue literal")
	}
	text, err := strconv.Unquote(literal.Value)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func checkSourceRefusal(t *testing.T, n ast.Node) bool {
	t.Helper()
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return true
	}
	name, ok := call.Fun.(*ast.Ident)
	if !ok || name.Name != "invalid" {
		return true
	}
	path := catalogueLiteral(t, call.Args[0])
	reason := catalogueLiteral(t, call.Args[1])
	e := &InvalidError{path: path, reason: reason}
	for text, limit := range map[string]int{e.Reason(): 80, e.Remedy(): 120} {
		if err := publicerr.CheckMessage(text, limit); err != nil {
			t.Error(path, err, text)
		}
	}
	return true
}
