package admissioncontract

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// ChildSpecDigest compares a scheduled child's spec with its sealed admission.
// Shutdown is mutable native control. All other spec fields remain frozen;
// child identity uses labelsFrom and needs no substitution inside the spec.
func ChildSpecDigest(spec wf.WorkflowSpec, shutdown wf.ShutdownStrategy) (string, error) {
	observed := spec.DeepCopy()
	observed.Shutdown = shutdown
	body, err := sourcecontract.CanonicalJSON(observed)
	if err != nil {
		return "", err
	}
	return sourcecontract.Digest(body), nil
}
