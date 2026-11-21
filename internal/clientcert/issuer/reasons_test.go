package issuer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func issuerCatalogueValid(src []byte) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "reasons.go", src, 0)
	if err != nil {
		return false
	}
	valid := true
	ast.Inspect(file, func(n ast.Node) bool {
		c, ok := n.(*ast.CompositeLit)
		if !ok || len(c.Elts) != 3 {
			return true
		}
		if _, ok := c.Elts[0].(*ast.SelectorExpr); !ok {
			return true
		}
		for i, limit := range []int{80, 120} {
			v, ok := c.Elts[i+1].(*ast.BasicLit)
			if !ok || v.Kind != token.STRING {
				valid = false
				continue
			}
			text, err := strconv.Unquote(v.Value)
			if err != nil || publicerr.CheckMessage(text, limit) != nil {
				valid = false
			}
		}
		return true
	})
	return valid
}
func TestIssuerReasonCatalog(t *testing.T) {
	src, err := os.ReadFile("reasons.go")
	if err != nil {
		t.Fatal(err)
	}
	if !issuerCatalogueValid(src) {
		t.Fatal("nonliteral or overlong diagnostic")
	}
	for _, d := range diagnostics() {
		if err := publicerr.CheckMessage(d.reason, 80); err != nil {
			t.Fatal(err)
		}
		if err := publicerr.CheckMessage(d.remedy, 120); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		old   string
		limit int
	}{{`"Authority dependencies are missing."`, 80}, {`"Supply entropy and a clock."`, 120}} {
		good := strings.Replace(string(src), tc.old, strconv.Quote(strings.Repeat("x", tc.limit)), 1)
		if !issuerCatalogueValid([]byte(good)) {
			t.Fatal("N catalogue refused")
		}
		bad := strings.Replace(string(src), tc.old, strconv.Quote(strings.Repeat("x", tc.limit+1)), 1)
		if issuerCatalogueValid([]byte(bad)) {
			t.Fatal("N+1 catalogue accepted")
		}
	}
	bad := strings.Replace(string(src), `"Authority dependencies are missing."`, `"Authority dependencies are missing." + ""`, 1)
	if issuerCatalogueValid([]byte(bad)) {
		t.Fatal("nonliteral diagnostic accepted")
	}
}
