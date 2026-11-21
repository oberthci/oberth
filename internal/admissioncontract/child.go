package admissioncontract

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// ScheduledChild binds a retained C4 admission to the native child it produced.
// It preserves the admitted spec, plan and authority without admitting again.
func (v PreparedView) ScheduledChild(child *wf.Workflow) (*PreparedAdmission, error) {
	if child == nil || child.Name == "" {
		return nil, &SealError{Path: "child", reason: childBindingReason}
	}
	sealed, err := Seal(v)
	if err != nil {
		return nil, err
	}
	if sealed.view.Digest != v.Digest || sealed.view.SpecDigest != v.SpecDigest {
		return nil, &SealError{Path: "child", reason: childDigestReason}
	}
	digest, err := ChildSpecDigest(child.Spec, v.Workflow.Spec.Shutdown)
	if err != nil || digest != v.SpecDigest || child.Namespace != v.Identity.Namespace {
		return nil, &SealError{Path: "child", reason: childSpecReason}
	}
	sealedSpec := v.Workflow.Spec.DeepCopy()
	v.RunID, v.Workflow = child.Name, child.DeepCopy()
	v.Workflow.Spec = *sealedSpec
	v.Workflow.Status = wf.WorkflowStatus{}
	return Seal(v)
}

const (
	childBindingReason = "scheduled admission does not match"
	childDigestReason  = "retained admission digest differs"
	childSpecReason    = "scheduled child spec differs"
)
