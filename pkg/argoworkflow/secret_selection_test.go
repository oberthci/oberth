package argoworkflow

import (
	"context"
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
)

func secretSelectionRequest(t *testing.T, paths string) admissioncontract.PrepareRequest {
	t.Helper()
	wf := simpleWorkflow()
	declareSecretPaths(&wf.Spec.Templates[0], paths)
	wf.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec"}
	return requestFor(t, wf)
}

func TestPreparationSelectsExactSecretGrantsBeforeWildcard(t *testing.T) {
	t.Parallel()
	exact, wildcard := clusterGrant("main", "secret", "public/key"), clusterGrant("*", "secret", "public/key")
	exact.Revision, wildcard.Revision = "exact", "wildcard"
	unverified := exact
	unverified.State = admissioncontract.StateUnknown
	for _, tc := range []struct {
		name   string
		grants []admissioncontract.GrantEvidence
		want   admissioncontract.GrantEvidence
	}{
		{"exact first", []admissioncontract.GrantEvidence{exact, wildcard}, exact},
		{"wildcard first", []admissioncontract.GrantEvidence{wildcard, exact}, exact},
		{"only wildcard", []admissioncontract.GrantEvidence{wildcard}, wildcard},
		{"unverified exact", []admissioncontract.GrantEvidence{unverified, wildcard}, wildcard},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := secretSelectionRequest(t, "public/key")
			r.Grants = tc.grants
			hooks := identityHooks()
			hooks.LoginAudience = "login"
			var selected admissioncontract.TransformInput
			hooks.TransformNative = func(_ context.Context, _ *wfv1.Workflow, input admissioncontract.TransformInput) error {
				selected = input
				return nil
			}
			if _, err := Prepare(t.Context(), r, Policy{}, hooks); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(selected.SecretGrants["main"], []admissioncontract.GrantEvidence{tc.want}) || selected.CredentialSteps["main"] != tc.want.Step {
				t.Fatalf("selected grants=%+v, step=%q; want %+v", selected.SecretGrants["main"], selected.CredentialSteps["main"], tc.want)
			}
		})
	}
}

func TestWildcardSecretGrantCannotFillExactStepGap(t *testing.T) {
	t.Parallel()
	r := secretSelectionRequest(t, "public/key,public/other")
	r.Grants = []admissioncontract.GrantEvidence{
		clusterGrant("main", "secret", "public/other"),
		clusterGrant("*", "secret", "public/key"),
	}
	hooks := identityHooks()
	hooks.LoginAudience = "login"
	transforms := 0
	hooks.TransformNative = func(context.Context, *wfv1.Workflow, admissioncontract.TransformInput) error {
		transforms++
		return nil
	}
	prepared, err := Prepare(t.Context(), r, Policy{}, hooks)
	const want = `.oberth/build.yaml: spec.templates["main"].secrets[public/key]: credential declaration has no exact operator grant; oberth secret put --repo 'forge/org/repo' --file '.oberth/build.yaml' --step 'main' 'public/key' < <value-file>`
	if err == nil || err.Error() != want || prepared != nil || transforms != 0 {
		t.Fatalf("wildcard filled exact grant gap: sealed=%v, error=%v, transforms=%d; want %s", prepared != nil, err, transforms, want)
	}
	r.Grants = append(r.Grants, clusterGrant("main", "secret", "public/key"))
	prepared, err = Prepare(t.Context(), r, Policy{}, hooks)
	if err != nil || prepared == nil || transforms != 1 {
		t.Fatalf("exact grant did not admit the same document: sealed=%v, error=%v, transforms=%d", prepared != nil, err, transforms)
	}
}
