package cronsync

import (
	"strconv"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/nativebinding"
)

// VerifyChild checks an observed child against committed custody without requiring
// a live parent. Only a durable proof permits this orphan recovery path.
func (c *Syncer) VerifyChild(child *wf.Workflow, proof nativebinding.ChildBinding) error {
	if child == nil || child.Namespace != c.config.Namespace || !childFacts(child, proof.ScheduleEntry) {
		return &ChildRefusal{}
	}
	owner := meta.GetControllerOf(child)
	if child.Labels[nativebinding.NativeCronLabel] != proof.Parent.Name || child.Labels[nativebinding.RepositoryLabel] != strconv.FormatInt(proof.Run.RepositoryID, 10) {
		return &ChildRefusal{}
	}
	observed := proof
	if owner != nil {
		if !controllerOwner(child, owner) {
			return &ChildRefusal{}
		}
		observed.Parent = nativebinding.Object{Namespace: child.Namespace, Name: owner.Name, UID: string(owner.UID)}
	}
	observed.Run.Workflow = nativebinding.Object{Namespace: child.Namespace, Name: child.Name, UID: string(child.UID)}
	observed.Run.RunID = child.Name
	observed.Run.Ref = child.Annotations[nativebinding.RefAnnotation]
	observed.Run.Revision.Commit = child.Labels[nativebinding.CommitLabel]
	observed.CreatedAt = child.CreationTimestamp.Time
	return proof.Verify(observed)
}
