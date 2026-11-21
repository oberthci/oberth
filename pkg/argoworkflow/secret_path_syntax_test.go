package argoworkflow

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func TestSecretDeclarationUsesGrantPathSyntax(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"a/key", "A_1/key-name.txt", ".hidden/key", strings.Repeat("a", 1024), ".", "..", "a..b/key", "a/../b", "a//b", "a/./b", "a/", "/a", "a b", "a\\b", "a?b", "a#b", "é/key", strings.Repeat("a", 1025)} {
		w := simpleWorkflow()
		declareSecretPaths(&w.Spec.Templates[0], path)
		r := requestFor(t, w)
		r.Mode, r.IdentityRequest.Mode = admissioncontract.ModeInspect, admissioncontract.ModeInspect
		result, err := Prepare(t.Context(), r, Policy{}, admissioncontract.PrepareHooks{OfflineValidation: true})
		if (err == nil) != workflowpolicy.ValidSecretPath(path) {
			t.Fatal("declaration differs", path, err)
		}
		if err != nil && result != nil {
			t.Fatal("refusal sealed workflow")
		}
	}
}
