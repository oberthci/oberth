// Package specsize measures only the submitted native spec for source delivery.
//
// Spec: docs/plans/c4-admission-rules-and-plan.md#invariants-and-budgets
package specsize

import (
	"encoding/json"
	"errors"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// Submitted returns encoded spec size, excluding metadata and status.
func Submitted(wf *wfv1.Workflow) (int, error) {
	if wf == nil {
		return 0, errors.New("workflow is required; provide the submitted Workflow")
	}
	raw, err := json.Marshal(wf.Spec)
	return len(raw), err
}
