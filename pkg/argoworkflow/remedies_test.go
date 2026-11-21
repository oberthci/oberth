package argoworkflow

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/model"
)

func TestSecretHelperRequiresPathsDeclaration(t *testing.T) {
	t.Parallel()
	for _, declared := range []string{"", "  "} {
		w := simpleWorkflow()
		declareSecretPaths(&w.Spec.Templates[0], declared)
		w.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec", "--", "true"}
		result, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
		findings := admissionFindings(t, err)
		if result != nil || len(findings) != 1 || findings[0].Path != `spec.templates["main"].metadata.annotations[`+SecretPathsAnnotation+"]" || findings[0].Code != "secret_paths" || findings[0].Command != "" {
			t.Fatal(result, findings)
		}
	}
}

func TestReleaseResourceRemedyDoesNotAskForGrant(t *testing.T) {
	t.Parallel()
	for _, granted := range []bool{false, true} {
		w, p := resourceWorkflow("apiVersion: example.test/v1\nkind: Widget\nmetadata: {name: deploy, namespace: resources}")
		if !granted {
			p.Capability = nil
		}
		p.Release = true
		r := requestFor(t, w)
		r.Trigger = model.TriggerTag
		result, err := Prepare(t.Context(), r, p.Policy, identityHooks())
		findings := admissionFindings(t, err)
		if result != nil || len(findings) != 1 || findings[0].Code != "release_resource" || findings[0].Remedy != "Move this step to the build pipeline." {
			t.Fatal(result, findings)
		}
	}
}

func TestAdmissionRemediesAreWholeAndBounded(t *testing.T) {
	t.Parallel()
	for code, m := range messages() {
		first, _ := utf8.DecodeRuneInString(m.remedy)
		if len(m.reason) > 80 || len(m.remedy) > 120 || !unicode.IsUpper(first) || !strings.HasSuffix(m.remedy, ".") {
			t.Fatal(code, m)
		}
	}
}
