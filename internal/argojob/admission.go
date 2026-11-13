package argojob

import (
	"errors"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// ValidateDocument runs Build's document-only isolation checks after
// argoworkflow.Admit. It normalizes a copy, leaving the caller's
// document unchanged. It does not check server state such as approvals, grants,
// identities or admission receipts.
func ValidateDocument(workflow *wfv1.Workflow, trigger pipeline.Trigger) error {
	if workflow == nil {
		return &argoworkflow.AdmissionError{Cause: errors.New("argojob: no Workflow to validate")}
	}
	return normalizeAndValidateDocument(workflow.DeepCopy(), trigger)
}

// Build retains these normalizations for subsequent server injection. Offline
// validation uses the same path on a copy, without assigning a server identity.
func normalizeAndValidateDocument(workflow *wfv1.Workflow, trigger pipeline.Trigger) error {
	argoworkflow.InheritTokenOptOut(workflow)
	normalizeContainerSetMounts(workflow)
	if err := validateTemplateIsolation(workflow, trigger); err != nil {
		return &argoworkflow.AdmissionError{Cause: err}
	}
	return nil
}
