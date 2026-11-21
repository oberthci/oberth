package argoworkflow

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/stepmodel/steptest"
)

func TestStatusReasonsAreShortCatalogueLiterals(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("errors.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "errors.go", raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	catalogues, entries := 0, 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name.Name == "messages" {
			entries += catalogueLiterals(t, fn, sharedCatalogueBindings(t, fn))
			catalogues++
		}
		if slices.Contains([]string{"literalMessages", "parameterMessages", "nativeMessages", "grantMessages"}, fn.Name.Name) {
			entries += catalogueLiterals(t, fn, nil)
			catalogues++
		}
		if fn.Name.Name == "Reason" {
			assertReasonLiteral(t, fn)
		}
	}
	if catalogues != 5 || entries != len(messages()) {
		t.Fatal("catalogue coverage mismatch", catalogues, entries, len(messages()))
	}
}
func assertReasonLiteral(t *testing.T, fn *ast.FuncDecl) {
	t.Helper()
	if len(fn.Body.List) != 1 {
		t.Fatal("reason must return one literal")
	}
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		t.Fatal("reason must return one literal")
	}
	reason, ok := stringLiteral(ret.Results[0])
	if !ok {
		t.Fatal("non-literal status reason")
	}
	steptest.AssertReasons(t, reason)
}

func catalogueLiterals(t *testing.T, fn *ast.FuncDecl, shared map[string]bool) int {
	t.Helper()
	count := 0
	ast.Inspect(fn, func(node ast.Node) bool {
		entry, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if entry.Type != nil {
			id, named := entry.Type.(*ast.Ident)
			if !named || id.Name != "message" {
				return true
			}
		}
		for _, expr := range entry.Elts {
			if _, ok := stringLiteral(expr); !ok {
				if id, ok := expr.(*ast.Ident); ok && shared[id.Name] {
					continue
				}
				t.Fatal("non-literal catalogue entry")
			}
		}
		count++
		return true
	})
	return count
}
func stringLiteral(expr ast.Expr) (string, bool) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}
func TestNonLiteralReasonControlIsRejected(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"nativeText", "\"prefix\" + nativeText", "format(nativeText)"} {
		expr, err := parser.ParseExpr(source)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := stringLiteral(expr); ok {
			t.Fatal("non-literal reason accepted")
		}
	}
}

func sharedCatalogueBindings(t *testing.T, fn *ast.FuncDecl) map[string]bool {
	t.Helper()
	binding, ok := fn.Body.List[0].(*ast.AssignStmt)
	if !ok || len(binding.Lhs) != 2 || len(binding.Rhs) != 1 {
		t.Fatal("shared catalogue binding changed")
	}
	call, ok := binding.Rhs[0].(*ast.CallExpr)
	if !ok {
		t.Fatal("shared catalogue must call its owner")
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "SecretStoreConfiguration" {
		t.Fatal("unexpected shared catalogue owner")
	}
	owner, ok := selector.X.(*ast.Ident)
	if !ok || owner.Name != "runtimeconfig" {
		t.Fatal("unexpected shared catalogue package")
	}
	assertSharedCatalogueLiterals(t)
	names := map[string]bool{}
	for _, expr := range binding.Lhs {
		id, ok := expr.(*ast.Ident)
		if !ok {
			t.Fatal("shared catalogue binding must name its literals")
		}
		names[id.Name] = true
	}
	return names
}

func TestSharedConfigurationMessageAndFindingJoin(t *testing.T) {
	t.Parallel()
	reason, remedy := runtimeconfig.SecretStoreConfiguration()
	message := messages()["store_config"]
	if message.reason != strings.TrimSuffix(reason, ".") || message.remedy != remedy {
		t.Fatal("shared configuration message differs")
	}
	err := &AdmissionError{Findings: []admissioncontract.Finding{finding("spec.templates[main]", "store_config")}}
	if strings.Contains(err.Error(), ".;") || !strings.Contains(err.Error(), message.reason+"; "+remedy) {
		t.Fatal("finding punctuation differs", err)
	}
}

func TestEveryFindingJoinsWithoutDoubledPunctuation(t *testing.T) {
	t.Parallel()
	for code := range messages() {
		t.Run(code, func(t *testing.T) {
			f := finding("spec.templates[main]", code)
			if joined := f.Reason + "; " + f.Remedy; strings.Contains(joined, ".;") {
				t.Fatal("stored finding has doubled punctuation", joined)
			}
			if err := (&AdmissionError{Findings: []admissioncontract.Finding{f}}).Error(); strings.Contains(err, ".;") {
				t.Fatal("rendered finding has doubled punctuation", err)
			}
		})
	}
}

func assertSharedCatalogueLiterals(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile("../../internal/runtimeconfig/secretstore.go")
	if err != nil {
		t.Fatal(err)
	}
	source, err := parser.ParseFile(token.NewFileSet(), "secretstore.go", raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, decl := range source.Decls {
		owned, ok := decl.(*ast.FuncDecl)
		if !ok || owned.Name.Name != "SecretStoreConfiguration" {
			continue
		}
		found = true
		returned, ok := owned.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(owned.Body.List) != 1 || len(returned.Results) != 2 {
			t.Fatal("shared catalogue must return two literals")
		}
		for _, expr := range returned.Results {
			if _, ok := stringLiteral(expr); !ok {
				t.Fatal("shared catalogue contains non-literal text")
			}
		}
	}
	if !found {
		t.Fatal("shared catalogue owner missing")
	}
}
