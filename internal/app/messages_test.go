package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

func TestMessageCatalogue(t *testing.T) {
	t.Parallel()
	for key, m := range messages() {
		if m.reason == "" || len(m.reason) > 40 || len(m.remedy) > 120 || strings.HasSuffix(m.reason, "...") || strings.HasSuffix(m.remedy, "...") {
			t.Fatalf("message %s: %+v", key, m)
		}
	}
	file, err := parser.ParseFile(token.NewFileSet(), "messages.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		f, ok := n.(*ast.FuncDecl)
		if !ok || f.Name.Name != "messages" {
			return true
		}
		assertLiterals(t, f.Body)
		return false
	})
}
func assertLiterals(t *testing.T, body *ast.BlockStmt) {
	t.Helper()
	ast.Inspect(body, func(n ast.Node) bool {
		item, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		values, ok := item.Value.(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, v := range values.Elts {
			literal, ok := v.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Fatal("nonliteral message")
			}
			if _, err := strconv.Unquote(literal.Value); err != nil {
				t.Fatal(err)
			}
		}
		return true
	})
}

func TestReleaseHeadRemedyNamesTheRetryAction(t *testing.T) {
	if got := messages()["release_head"].remedy; got != "Push the tag again after the default branch is readable." {
		t.Fatal("release head remedy is not actionable", got)
	}
}

func TestReleaseOldRemedyNamesCancelAction(t *testing.T) {
	t.Parallel()
	refused := refusal("release_old", nil)
	want := "Choose a higher version, or cancel the in-flight release: oberth run cancel <run> --force."
	if refused.Remedy() != want || !strings.Contains(refused.Error(), want) {
		t.Fatal("release floor refusal must show the whole cancellation remedy", refused.Error())
	}
}
