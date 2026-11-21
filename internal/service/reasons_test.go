package service

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel/steptest"
)

func checkCatalogue(src []byte) error {
	file, err := parser.ParseFile(token.NewFileSet(), "reasons.go", src, 0)
	if err != nil {
		return err
	}
	ast.Inspect(file, func(n ast.Node) bool {
		if err != nil {
			return false
		}
		err = checkCatalogueNode(n)
		return err == nil
	})
	return err
}
func literalMessage(expr ast.Expr, limit int) error {
	value, ok := expr.(*ast.BasicLit)
	if !ok || value.Kind != token.STRING {
		return fmt.Errorf("message must be a string literal")
	}
	text, err := strconv.Unquote(value.Value)
	if err != nil {
		return err
	}
	return publicerr.CheckMessage(text, limit)
}
func TestReasonCatalog(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("reasons.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkCatalogue(src); err != nil {
		t.Fatal(err)
	}
	steptest.AssertReasons(t, reasonQueued, reasonRunning, reasonPassed, reasonFailed, reasonCancelled, reasonSkipped, reasonPending, reasonUnknown, reasonParked, reasonDeleted, reasonPurged, reasonRelease, reasonHeld)
	for i, d := range diagnostics() {
		if err := publicerr.CheckMessage(d.reason, 80); err != nil {
			t.Fatal(err)
		}
		if err := publicerr.CheckMessage(d.remedy, 120); err != nil {
			t.Fatal(err)
		}
		if refuse(problem(i)).Reason() != d.reason {
			t.Fatal("catalogue lookup mismatch")
		}
	}
}
func TestReasonCatalogAcceptsBoundAndRejectsNonLiteral(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("reasons.go")
	if err != nil {
		t.Fatal(err)
	}
	replace := func(value string) []byte {
		return []byte(strings.Replace(string(src), `"waiting for execution"`, value, 1))
	}
	if err := checkCatalogue(replace(strconv.Quote(strings.Repeat("x", 40)))); err != nil {
		t.Fatal("40 refused", err)
	}
	for _, value := range []string{strconv.Quote(strings.Repeat("x", 41)), `"waiting for execution" + ""`, `"waiting..."`} {
		if err := checkCatalogue(replace(value)); err == nil {
			t.Fatalf("bad status catalogue accepted %s", value)
		}
	}
	for _, tc := range []struct {
		old string
		n   int
	}{{`"The requested object was not found."`, 80}, {`"Select a visible repository or run."`, 120}} {
		good := []byte(strings.Replace(string(src), tc.old, strconv.Quote(strings.Repeat("x", tc.n)), 1))
		if err := checkCatalogue(good); err != nil {
			t.Fatal(err)
		}
		bad := []byte(strings.Replace(string(src), tc.old, strconv.Quote(strings.Repeat("x", tc.n+1)), 1))
		if err := checkCatalogue(bad); err == nil {
			t.Fatal("overlong refusal/remedy accepted")
		}
	}
}

func checkCatalogueNode(n ast.Node) error {
	var err error
	if v, ok := n.(*ast.ValueSpec); ok {
		return checkReasonConstants(v)
	}
	if c, ok := n.(*ast.CompositeLit); ok && len(c.Elts) == 3 {
		if _, ok := c.Elts[0].(*ast.SelectorExpr); ok {
			err = literalMessage(c.Elts[1], 80)
			if err == nil {
				err = literalMessage(c.Elts[2], 120)
			}
		}
	}
	return err
}

func checkReasonConstants(v *ast.ValueSpec) error {
	var err error
	for i, name := range v.Names {
		if strings.HasPrefix(name.Name, "warning") {
			if i >= len(v.Values) {
				return fmt.Errorf("%s has no literal", name.Name)
			}
			if err = literalMessage(v.Values[i], 120); err != nil {
				return fmt.Errorf("%s: %w", name.Name, err)
			}
		}
		if strings.HasPrefix(name.Name, "reason") {
			if i >= len(v.Values) {
				err = fmt.Errorf("%s has no literal", name.Name)
				return err
			}
			err = literalMessage(v.Values[i], 40)
			if err != nil {
				return err
			}
		}
	}
	return err
}

// A grant warning is a reason of at most 40 characters and a remedy of at
// most 120 with a typical repository name, each one sentence.
func TestGrantWarningsFitTheMessageRules(t *testing.T) {
	t.Parallel()
	typical := "github/example-organization/example-repository"
	for _, m := range []struct {
		text  string
		limit int
	}{{warningAuthorityIncomplete, 40}, {warningAuthorityRetry, 120}, {warningSchedulesPaused, 40}, {fmt.Sprintf(warningSchedulesRetry, typical), 120}} {
		if err := publicerr.CheckMessage(m.text, m.limit); err != nil {
			t.Fatal(m.text, err)
		}
	}
	src, err := os.ReadFile("reasons.go")
	if err != nil {
		t.Fatal(err)
	}
	long := strconv.Quote(strings.Repeat("x", 121))
	if err := checkCatalogue([]byte(strings.Replace(string(src), strconv.Quote(warningAuthorityRetry), long, 1))); err == nil {
		t.Fatal("an overlong warning constant passed the catalogue check")
	}
}

func TestInvitationAddressRefusalIsCatalogued(t *testing.T) {
	entry := diagnostics()[invitationAddressMismatch]
	if entry.code != publicerr.InvalidInput || entry.reason != "Invitation address must match the public address." || entry.remedy != "Correct server_url in the selected profile's installation.json, then run oberth invite <upstream>." {
		t.Fatal("invitation address refusal is absent from the bounded catalogue", entry)
	}
}

func TestIssueReadAloneMessageFitsLargestNumber(t *testing.T) {
	err := issueReadAlone(9223372036854775806)
	if e := publicerr.CheckMessage(err.Reason(), 80); e != nil {
		t.Fatal(e)
	}
	if e := publicerr.CheckMessage(err.Remedy(), 120); e != nil {
		t.Fatal(e)
	}
}
