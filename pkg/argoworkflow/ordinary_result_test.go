package argoworkflow

import (
	"context"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	core "k8s.io/api/core/v1"
)

func TestOrdinaryPreparationSkipsDormantResultScan(t *testing.T) {
	t.Parallel()
	if admissioncontract.AssistEnabled {
		t.Fatal("ordinary preparation requires the dormant assist gate")
	}
	w := simpleWorkflow()
	w.Spec.Arguments.Parameters = []wf.Parameter{{Name: "message", Value: wf.AnyStringPtr("{{tasks.producer.outputs.result}}")}}
	w.Spec.Templates[0].Outputs.Parameters = []wf.Parameter{{Name: "value", ValueFrom: &wf.ValueFrom{Path: "/tmp/value"}}}
	selector := &core.SecretKeySelector{LocalObjectReference: core.LocalObjectReference{Name: "private"}, Key: "dummy"}
	kind := holdSecretRoute(w, selector, "env")
	r := requestFor(t, w)
	r.Grants = []admissioncontract.GrantEvidence{clusterGrant("*", kind, "private")}
	hooks := identityHooks()
	transformed := false
	hooks.TransformNative = func(_ context.Context, _ *wf.Workflow, input admissioncontract.TransformInput) error {
		transformed = true
		assertOrdinaryResumeFacts(t, input.Plan)
		return nil
	}
	prepared, err := Prepare(t.Context(), r, Policy{}, hooks)
	if err != nil {
		t.Fatal(err)
	}
	if !transformed {
		t.Fatal("native transformation was not exercised")
	}
	assertOrdinaryResumeFacts(t, prepared.View().Plan)
}

func assertOrdinaryResumeFacts(t *testing.T, plan admissioncontract.Plan) {
	t.Helper()
	if len(plan.Entries) != 1 {
		t.Fatal("ordinary preparation lost its plan", plan)
	}
	e := plan.Entries[0]
	if e.ResultConsumed || !e.SecretRoute || !e.OutputParameters {
		t.Fatal("ordinary preparation scanned dormant results or lost retry safety facts", e)
	}
}
