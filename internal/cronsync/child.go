package cronsync

import (
	"context"
	"strconv"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// ChildBinding verifies a live native parent and the child's frozen facts.
// Unrelated outer labels and annotations are not authority and are ignored.
// The booleans report a child and whether its admission is the live parent's current one.
// C5 completes SpecSHA256 and validates the binding before C7 persists it.
func (c *Syncer) ChildBinding(ctx context.Context, child *wfv1.Workflow) (nativebinding.ChildBinding, bool, bool, error) {
	if child == nil || child.Labels[nativebinding.NativeCronLabel] == "" {
		return nativebinding.ChildBinding{}, false, false, nil
	}
	if child.Namespace != c.config.Namespace {
		return nativebinding.ChildBinding{}, false, false, &ChildRefusal{}
	}
	owner := metav1.GetControllerOf(child)
	if !controllerOwner(child, owner) {
		return nativebinding.ChildBinding{}, false, false, &ChildRefusal{}
	}
	parent, err := c.config.Client.Get(ctx, owner.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nativebinding.ChildBinding{}, false, false, &ChildRefusal{cause: err}
	}
	if err != nil {
		return nativebinding.ChildBinding{}, false, false, problem(nativeFailure, err)
	}
	_, entry, ok := owned(parent)
	if !ok || parent.Namespace != child.Namespace || parent.UID != owner.UID || !nativebinding.ChildName(child.Name, parent.Name) || !childFacts(child, entry) {
		return nativebinding.ChildBinding{}, false, false, &ChildRefusal{}
	}
	binding, found, err := c.childProof(ctx, child, parent)
	return binding, found, currentChildAdmission(child, parent), err
}

func currentChildAdmission(child *wfv1.Workflow, parent *wfv1.CronWorkflow) bool {
	metadata := parent.Spec.WorkflowMetadata
	return metadata != nil && metadata.Annotations[nativebinding.AdmissionAnnotation] == child.Annotations[nativebinding.AdmissionAnnotation]
}
func (c *Syncer) childProof(ctx context.Context, child *wfv1.Workflow, parent *wfv1.CronWorkflow) (nativebinding.ChildBinding, bool, error) {
	id, entry, _ := owned(parent)
	repositories, err := c.config.Repositories(ctx)
	if err != nil {
		return nativebinding.ChildBinding{}, false, problem(catalogFailure, err)
	}
	for _, repo := range repositories {
		if repo.ID != id {
			continue
		}
		if child.Labels[nativebinding.RepositoryLabel] != strconv.FormatInt(id, 10) {
			return nativebinding.ChildBinding{}, false, &ChildRefusal{}
		}
		if err := sourcecontract.ValidatePublicRef(child.Annotations[nativebinding.RefAnnotation]); err != nil {
			return nativebinding.ChildBinding{}, false, &ChildRefusal{cause: err}
		}
		revision := sourcecontract.Revision{Repository: repo.Qualified, Commit: child.Labels[nativebinding.CommitLabel]}
		if err := revision.Validate(); err != nil {
			return nativebinding.ChildBinding{}, false, &ChildRefusal{cause: err}
		}
		binding := nativebinding.ChildBinding{
			Run: nativebinding.RunBinding{
				Workflow:     nativebinding.Object{Namespace: child.Namespace, Name: child.Name, UID: string(child.UID)},
				RepositoryID: id,
				RunID:        child.Name,
				Revision:     revision,
				Ref:          child.Annotations[nativebinding.RefAnnotation],
				Trigger:      "schedule",
				Identity:     child.Annotations[nativebinding.AdmissionAnnotation],
			},
			Parent:        nativebinding.Object{Namespace: parent.Namespace, Name: parent.Name, UID: string(parent.UID)},
			ScheduleEntry: entry,
			CreatedAt:     child.CreationTimestamp.Time,
		}
		return binding, true, nil
	}
	return nativebinding.ChildBinding{}, false, &ChildRefusal{}
}
func childFacts(child *wfv1.Workflow, entry string) bool {
	if !frozenFacts(child, entry) || !childTier(child) {
		return false
	}
	if value := child.Labels[nativebinding.RunLabel]; value != "" && value != child.Name {
		return false
	}
	metadata := child.Spec.WorkflowMetadata
	return metadata != nil && metadata.LabelsFrom[nativebinding.RunLabel].Expression == "workflow.name"
}

func controllerOwner(child *wfv1.Workflow, owner *metav1.OwnerReference) bool {
	return owner != nil && owner.APIVersion == wfv1.SchemeGroupVersion.String() && owner.Kind == "CronWorkflow" && owner.Name == child.Labels[nativebinding.NativeCronLabel] && owner.UID != "" && child.UID != "" && !child.CreationTimestamp.IsZero()
}
func childTier(child *wfv1.Workflow) bool {
	tier := nativebinding.TierValue(false)
	return child.Labels[nativebinding.EngineLabel] == nativebinding.EngineValue && child.Labels[nativebinding.TriggerLabel] == tier && child.Labels[nativebinding.TierLabel] == tier
}
func frozenFacts(child *wfv1.Workflow, entry string) bool {
	expected := map[string]string{"oberth-ref": child.Annotations[nativebinding.RefAnnotation], "oberth-sha": child.Labels[nativebinding.CommitLabel], triggerParameter: "schedule", scheduleParameter: entry}
	for key, value := range expected {
		if value == "" || parameter(&child.Spec, key) != value {
			return false
		}
	}
	return true
}
