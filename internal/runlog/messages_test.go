package runlog

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

// Every generated reason and remedy is a literal in its package catalogue.
func TestComponentMessagesFitOwnerLimits(t *testing.T) {
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, "messages.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	ast.Inspect(file, func(node ast.Node) bool {
		value, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		checkMessageValues(t, set, value, seen)
		return true
	})
}
func checkMessageValues(t *testing.T, set *token.FileSet, value *ast.ValueSpec, seen map[string]string) {
	t.Helper()
	for i, name := range value.Names {
		limit := 0
		if strings.HasSuffix(name.Name, "Reason") {
			limit = 40
		}
		if strings.HasSuffix(name.Name, "Remedy") {
			limit = 120
		}
		if limit == 0 {
			continue
		}
		if i >= len(value.Values) {
			t.Errorf("%s: message must be a literal", name.Name)
			continue
		}
		text := checkLiteralMessage(t, set, value.Values[i], limit)
		if previous, ok := seen[text]; ok {
			t.Errorf("%s duplicates %s", name.Name, previous)
		}
		seen[text] = name.Name
	}
}

func checkLiteralMessage(t *testing.T, set *token.FileSet, expression ast.Expr, limit int) string {
	t.Helper()
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		t.Errorf("%s: message must be a string literal", set.Position(expression.Pos()))
		return ""
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		t.Fatal(err)
	}
	if err = publicerr.CheckMessage(value, limit); err != nil {
		t.Errorf("%s: %v", set.Position(expression.Pos()), err)
	}
	return value
}
