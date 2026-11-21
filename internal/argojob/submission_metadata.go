package argojob

import (
	"strconv"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
)

// stampSubmission supplies the same frozen facts to Create and to reusable
// schedule preparation. Child identities stay native expressions in the sealed spec.
func stampSubmission(w *wf.Workflow, v admissioncontract.PreparedView, repositoryID int64) {
	if w.Labels == nil {
		w.Labels = map[string]string{}
	}
	if w.Annotations == nil {
		w.Annotations = map[string]string{}
	}
	for k, val := range map[string]string{nativebinding.EngineLabel: nativebinding.EngineValue, nativebinding.RunLabel: v.RunID, nativebinding.RepositoryLabel: strconv.FormatInt(repositoryID, 10), nativebinding.CommitLabel: v.CommitSHA, nativebinding.TriggerLabel: string(v.Trigger)} {
		w.Labels[k] = val
	}
	for k, val := range map[string]string{nativebinding.RunIDAnnotation: v.RunID, nativebinding.RefAnnotation: v.Ref, nativebinding.IdentityAnnotation: v.SpecDigest} {
		w.Annotations[k] = val
	}
}

func stampChildIdentity(w *wf.Workflow) {
	delete(w.Labels, nativebinding.RunLabel)
	delete(w.Annotations, nativebinding.RunIDAnnotation)
	delete(w.Annotations, nativebinding.IdentityAnnotation)
	w.Labels[nativebinding.TriggerLabel] = nativebinding.TierValue(false)
	w.Labels[nativebinding.TierLabel] = nativebinding.TierValue(false)
	if w.Spec.WorkflowMetadata == nil {
		w.Spec.WorkflowMetadata = &wf.WorkflowMetadata{}
	}
	m := w.Spec.WorkflowMetadata
	if m.LabelsFrom == nil {
		m.LabelsFrom = map[string]wf.LabelValueFrom{}
	}
	m.LabelsFrom[nativebinding.RunLabel] = wf.LabelValueFrom{Expression: "workflow.name"}
}
