package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/stepmodel/steptest"
)

func TestStatusReasonsComeFromLiteralCatalogue(t *testing.T) {
	t.Parallel()
	catalogue := literalCatalogue(t)
	entries, err := os.ReadDir(".")
	requireOK(t, err)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, 0)
		requireOK(t, err)
		assertReasonAssignments(t, parsed, catalogue, entry.Name())
	}
}

func literalCatalogue(t *testing.T) map[string]bool {
	t.Helper()
	source, err := parser.ParseFile(token.NewFileSet(), "../stateport/reasons.go", nil, 0)
	requireOK(t, err)
	catalogue := map[string]bool{}
	ast.Inspect(source, func(node ast.Node) bool {
		value, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for index, name := range value.Names {
			if !strings.HasPrefix(strings.ToLower(name.Name), "reason") {
				continue
			}
			if index >= len(value.Values) {
				t.Fatal("status reason is not literal", name.Name)
			}
			literal, ok := value.Values[index].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Fatal("status reason is not literal", name.Name)
			}
			text, err := strconv.Unquote(literal.Value)
			requireOK(t, err)
			steptest.AssertReasons(t, text)
			catalogue[name.Name] = true
		}
		return true
	})
	if len(catalogue) != 7 {
		t.Fatal("catalogue did not cover generated statuses")
	}
	return catalogue
}

func assertReasonAssignments(t *testing.T, parsed *ast.File, catalogue map[string]bool, file string) {
	t.Helper()
	ast.Inspect(parsed, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for index, target := range assignment.Lhs {
			selector, ok := target.(*ast.SelectorExpr)
			if ok && selector.Sel.Name == "DisplayReason" && !catalogueReason(file, assignment, index, catalogue) {
				t.Fatal("status bypassed catalogue", file)
			}
		}
		return true
	})
}

// catalogueReason accepts a reason from the literal catalogue, a native verdict
// or the waiting reason of native progress.
func catalogueReason(file string, assignment *ast.AssignStmt, index int, catalogue map[string]bool) bool {
	if len(assignment.Rhs) != len(assignment.Lhs) {
		return nativeProgress(file, assignment.Rhs[0])
	}
	if nativeReasonProjection(file, assignment.Rhs[index]) {
		return true
	}
	source, ok := assignment.Rhs[index].(*ast.SelectorExpr)
	return ok && catalogue[source.Sel.Name]
}

func nativeReasonProjection(file string, e ast.Expr) bool {
	if file != "run_mutations.go" {
		return false
	}
	source, ok := e.(*ast.SelectorExpr)
	if !ok || source.Sel.Name != "Reason" {
		return false
	}
	input, ok := source.X.(*ast.Ident)
	return ok && input.Name == "update"
}

// nativeProgress is the waiting reason of native progress, which the store
// checks against the reason bounds before the run-status rule returns it.
func nativeProgress(file string, e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if file != "native_progress.go" || !ok {
		return false
	}
	source, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || source.Sel.Name != "Progress" {
		return false
	}
	pkg, ok := source.X.(*ast.Ident)
	return ok && pkg.Name == "stepmodel"
}
