package secretstore

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

func TestEveryRefusalUsesCatalogue(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkRefusalCatalogue(source); err != nil {
			t.Error(path, err)
		}
	}
}

func checkRefusalCatalogue(source []byte) error {
	node, err := parser.ParseFile(token.NewFileSet(), "refusal.go", source, 0)
	if err != nil {
		return err
	}
	catalogue := refusalCatalogue()
	forwarding := false
	ast.Inspect(node, func(n ast.Node) bool {
		if fn, ok := n.(*ast.FuncDecl); ok {
			forwarding = fn.Name.Name == "refuse"
		}
		if call, ok := n.(*ast.CallExpr); ok && err == nil {
			err = checkRefusalCall(call, catalogue, forwarding)
		}
		return err == nil
	})
	return err
}

func checkRefusalCall(call *ast.CallExpr, catalogue map[string][2]string, forwarding bool) error {
	name, ok := call.Fun.(*ast.Ident)
	if !ok || len(call.Args) == 0 {
		return nil
	}
	switch name.Name {
	case "refuse", "refusal":
	default:
		return nil
	}
	// Only the constructor forwards its code to the catalogue lookup.
	if forwarding && name.Name == "refusal" {
		if ident, ok := call.Args[0].(*ast.Ident); ok && ident.Name == "code" {
			return nil
		}
	}
	literal, ok := call.Args[0].(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return fmt.Errorf("%s needs a literal catalogue code", name.Name)
	}
	code, _ := strconv.Unquote(literal.Value)
	if _, found := catalogue[code]; !found {
		return fmt.Errorf("uncatalogued refusal %s", literal.Value)
	}
	return nil
}

func TestRefusalGuardRejectsUncataloguedCode(t *testing.T) {
	for _, tc := range []struct{ call, want string }{
		{`refuse("backup-new", nil)`, `uncatalogued refusal "backup-new"`},
		{`refusal("backup-new")`, `uncatalogued refusal "backup-new"`},
		{`refuse(code, nil)`, "needs a literal catalogue code"},
		{`refusal(code)`, "needs a literal catalogue code"},
	} {
		err := checkRefusalCatalogue([]byte("package secretstore\nfunc fixture() { " + tc.call + " }"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatal("uncatalogued refusal passed the guard or failed for another reason", tc.call, err)
		}
	}
}
