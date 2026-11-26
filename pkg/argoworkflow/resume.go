package argoworkflow

import (
	"encoding/json"
	"github.com/oberthci/oberth/internal/sourcecontract"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
)

// SameExecution permits only the source locator and run/commit identity to move.
// Grants, accounts, pins and all authored execution inputs remain sealed.
func SameExecution(a, b admissioncontract.PreparedView) bool {
	return sameResumeSpec(a.Workflow, b.Workflow) && sameResumePlan(a.Plan, b.Plan) &&
		sameResumeJSON(a.Grants, b.Grants) && sameResumeJSON(a.Identity, b.Identity) &&
		sameResumeJSON(a.Defaults, b.Defaults) && sameResumeJSON(a.Templates, b.Templates) && samePins(a, b)
}

func sameResumeSpec(a, b *wf.Workflow) bool {
	if a == nil || b == nil {
		return false
	}
	x, y := a.DeepCopy(), b.DeepCopy()
	for _, w := range []*wf.Workflow{x, y} {
		for i := range w.Spec.Arguments.Parameters {
			if w.Spec.Arguments.Parameters[i].Name == "oberth-source" {
				w.Spec.Arguments.Parameters[i].Value = nil
			}
		}
	}
	xb, xe := json.Marshal(x.Spec)
	yb, ye := json.Marshal(y.Spec)
	return xe == nil && ye == nil && string(xb) == string(yb) && sameResumeJSON(x.ObjectMeta, y.ObjectMeta)
}

func samePins(a, b admissioncontract.PreparedView) bool {
	x, y := a.Clone(), b.Clone()
	for _, v := range []*admissioncontract.PreparedView{&x, &y} {
		for i := range v.SourceBindings {
			if v.SourceBindings[i].Descriptor.Kind == "main" {
				v.SourceBindings[i] = sourcecontract.Binding{}
			}
		}
	}
	return sameResumeJSON(x.SourceBindings, y.SourceBindings)
}

// ResumeAdmission retains F's checkout and grants under the unchanged native spec.
// Callers must establish SameExecution before requesting this seal.
func ResumeAdmission(fresh *admissioncontract.PreparedAdmission, old admissioncontract.PreparedView) (*admissioncontract.PreparedAdmission, error) {
	v := fresh.View()
	v.Workflow = old.Workflow.DeepCopy()
	return admissioncontract.Seal(v)
}

func sameResumePlan(a, b admissioncontract.Plan) bool { return sameResumeJSON(a, b) }

func sameResumeJSON(a, b any) bool {
	x, xe := json.Marshal(a)
	y, ye := json.Marshal(b)
	return xe == nil && ye == nil && string(x) == string(y)
}
