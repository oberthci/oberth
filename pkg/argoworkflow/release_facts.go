package argoworkflow

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
)

func (g *graph) stampReleaseFacts(w *wf.Workflow, hooks admissioncontract.PrepareHooks) error {
	if !g.policy.Release {
		return nil
	}
	if hooks.ReleaseFacts == nil {
		return nil
	}
	facts, err := hooks.ReleaseFacts(g.ctx)
	if err != nil {
		return err
	}
	parameters := facts.Parameters()
	for _, name := range sortedKeys(parameters) {
		w.Spec.Arguments.Parameters = append(w.Spec.Arguments.Parameters, wf.Parameter{Name: name, Value: wf.AnyStringPtr(parameters[name])})
	}
	return nil
}

// Offline schema checks know the parameter names, but never produce run evidence.
func releaseSchemaParameters(w *wf.Workflow) *wf.Workflow {
	copy := w.DeepCopy()
	present := map[string]bool{}
	for _, p := range copy.Spec.Arguments.Parameters {
		present[p.Name] = true
	}
	for _, name := range sortedKeys((admissioncontract.ReleaseFacts{}).Parameters()) {
		if !present[name] {
			copy.Spec.Arguments.Parameters = append(copy.Spec.Arguments.Parameters, wf.Parameter{Name: name, Value: wf.AnyStringPtr("unverified")})
		}
	}
	return copy
}
