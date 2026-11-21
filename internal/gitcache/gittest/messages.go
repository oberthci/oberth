package gittest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

// Messages walks the entire catalogue and rejects computed reason/remedy text.
func Messages(t *testing.T, path string) {
	t.Helper()
	for _, err := range MessageErrors(path) {
		t.Error(err)
	}
}
func MessageErrors(path string) []error {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return []error{err}
	}
	var result []error
	count := 0
	ast.Inspect(f, func(n ast.Node) bool {
		value, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range value.Names {
			limit := messageLimit(name.Name)
			if limit == 0 {
				continue
			}
			count++
			if i >= len(value.Values) {
				result = append(result, fmt.Errorf("%s is not a literal", name))
				continue
			}
			if err := literalMessage(value.Values[i], limit); err != nil {
				result = append(result, fmt.Errorf("%s: %w", name, err))
			}
		}
		return true
	})
	if count == 0 {
		result = append(result, fmt.Errorf("empty message catalogue"))
	}
	return result
}
func messageLimit(name string) int {
	if strings.HasPrefix(name, "reason") {
		return 40
	}
	if strings.HasPrefix(name, "remedy") {
		return 120
	}
	return 0
}
func literalMessage(expr ast.Expr, limit int) error {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return fmt.Errorf("message is not a literal")
	}
	text, err := strconv.Unquote(literal.Value)
	if err != nil {
		return err
	}
	return publicerr.CheckMessage(text, limit)
}
