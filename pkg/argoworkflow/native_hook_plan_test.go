package argoworkflow

import (
	"context"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"testing"
)

func TestNativeHookReceivesDetachedAdmissionPlan(t *testing.T) {
	t.Parallel()
	hooks := identityHooks()
	seen := false
	hooks.TransformNative = func(_ context.Context, _ *wf.Workflow, in admissioncontract.TransformInput) error {
		if len(in.Plan.Entries) != 1 || in.Plan.Entries[0].Template != "main" || in.Plan.Entries[0].CredentialIntent {
			t.Fatal(in.Plan)
		}
		in.Plan.Entries[0].CredentialIntent = true
		in.Plan.Entries[0].HoldArmed = true
		seen = true
		return nil
	}
	p, err := Prepare(t.Context(), requestFor(t, simpleWorkflow()), Policy{}, hooks)
	if err != nil || !seen {
		t.Fatal(err, seen)
	}
	if p.View().Plan.Entries[0].CredentialIntent || !p.View().Plan.Entries[0].HoldArmed {
		t.Fatal("hook changed the sealed plan")
	}
}
