package argoworkflow

import (
	"context"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// Test fixtures compose policy, evidence and preparation hooks.
type admissionFixture struct {
	Policy
	admissioncontract.PrepareRequest
	admissioncontract.PrepareHooks
}

func admitFixture(ctx context.Context, wf *wfv1.Workflow, f admissionFixture) error {
	return f.admit(ctx, wf, f.PrepareRequest, f.PrepareHooks)
}
func planFixture(ctx context.Context, wf *wfv1.Workflow, f admissionFixture) (admissioncontract.Plan, error) {
	return plannedSteps(ctx, wf, f.PrepareRequest, f.Policy)
}
func prepareFixture(ctx context.Context, r admissioncontract.PrepareRequest, f admissionFixture, hooks admissioncontract.PrepareHooks) (*admissioncontract.PreparedAdmission, error) {
	hooks.LoginAudience = f.LoginAudience
	return Prepare(ctx, r, f.Policy, hooks)
}

func bindingHook(bindings ...sourcecontract.Binding) func(context.Context, sourcecontract.Descriptor) (sourcecontract.Binding, error) {
	next := 0
	return func(context.Context, sourcecontract.Descriptor) (sourcecontract.Binding, error) {
		if next >= len(bindings) {
			return sourcecontract.Binding{}, nil
		}
		binding := bindings[next]
		next++
		return binding, nil
	}
}
func sourceHooks(bindings ...sourcecontract.Binding) admissioncontract.PrepareHooks {
	hooks := identityHooks()
	hooks.BindSource = bindingHook(bindings...)
	return hooks
}

// admit checks all declarations. Reachability narrows source usage and the plan,
// never inspection of a field that can reach cluster data or Oberth identity.
func (p Policy) admit(ctx context.Context, wf *wfv1.Workflow, r admissioncontract.PrepareRequest, hooks admissioncontract.PrepareHooks) error {
	if wf == nil {
		return refusal("workflow", "document")
	}
	g := p.admitRequest(ctx, wf, r, hooks)
	return fileError(r.SelectedFile, g.err())
}

// plannedSteps walks native invocations in declaration order. Unresolved work
// stays visible as an expandable entry; representation bounds never limit Pods.
func plannedSteps(ctx context.Context, wf *wfv1.Workflow, r admissioncontract.PrepareRequest, p Policy) (admissioncontract.Plan, error) {
	if wf == nil {
		return admissioncontract.Plan{}, refusal("workflow", "document")
	}
	g := newGraph(ctx, wf, r, p)
	g.build()
	if err := g.err(); err != nil {
		return admissioncontract.Plan{}, err
	}
	g.selectSecretGrants()
	return g.plan(), nil
}
