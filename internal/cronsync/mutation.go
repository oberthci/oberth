package cronsync

import (
	"context"
	"github.com/argoproj/argo-workflows/v4/util/instanceid"
	"github.com/oberthci/oberth/internal/nativebinding"
	"maps"
	"reflect"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *Syncer) suspend(ctx context.Context, name string) error {
	return c.mutate(ctx, name, nil, func(object *wfv1.CronWorkflow) error {
		object.Spec.Suspend = true
		return nil
	})
}

func (c *Syncer) mutate(ctx context.Context, name string, create *wfv1.CronWorkflow, change func(*wfv1.CronWorkflow) error) error {
	var lastConflict error
	// Bound optimistic-conflict repair; the next event can retry the action.
	for range 3 {
		if err := ctx.Err(); err != nil {
			return err
		}
		old, err := c.config.Client.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return c.create(ctx, create, change)
		}
		if err != nil {
			return problem(nativeFailure, err)
		}
		if _, _, ok := owned(old); !ok || old.Namespace != c.config.Namespace {
			return problem(foreignParent, nil)
		}
		next := old.DeepCopy()
		if err := change(next); err != nil {
			return err
		}
		instanceid.NewService(c.config.InstanceID).Label(next)
		if reflect.DeepEqual(old, next) {
			return nil
		}
		_, err = c.config.Client.Update(ctx, next, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		lastConflict = err
		if !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			return problem(nativeFailure, err)
		}
	}
	return problem(pendingSync, lastConflict)
}
func (c *Syncer) create(ctx context.Context, desired *wfv1.CronWorkflow, change func(*wfv1.CronWorkflow) error) error {
	if desired == nil {
		return nil
	}
	next := desired.DeepCopy()
	if err := change(next); err != nil {
		return err
	}
	_, err := c.config.Client.Create(ctx, next, metav1.CreateOptions{})
	if err != nil {
		return problem(nativeFailure, err)
	}

	return nil
}
func (c *Syncer) apply(ctx context.Context, request Preparation, desired *wfv1.CronWorkflow) (string, error) {
	var previous string
	err := c.mutate(ctx, desired.Name, desired, func(object *wfv1.CronWorkflow) error {
		paused, err := c.config.PauseIntent(ctx, request.Repository.Qualified, request.Entry.Name)
		if err != nil {
			return problem(pauseUnavailable, err)
		}
		previous = preservePredecessor(object, desired.Spec.WorkflowMetadata.Annotations[nativebinding.AdmissionAnnotation])
		object.Spec = *desired.Spec.DeepCopy()
		object.Spec.Suspend = paused
		if object.Labels == nil {
			object.Labels = map[string]string{}
		}
		maps.Copy(object.Labels, desired.Labels)
		return nil
	})
	return previous, err
}

// The predecessor is published atomically with the spec, before store pruning.
func preservePredecessor(object *wfv1.CronWorkflow, current string) string {
	var previous string
	if metadata := object.Spec.WorkflowMetadata; metadata != nil {
		previous = metadata.Annotations[nativebinding.AdmissionAnnotation]
	}
	if previous == current {
		return object.Annotations[previousAdmissionAnnotation]
	}
	if object.Annotations == nil {
		object.Annotations = map[string]string{}
	}
	object.Annotations[previousAdmissionAnnotation] = previous
	return previous
}

func (c *Syncer) remove(ctx context.Context, object *wfv1.CronWorkflow) error {
	if object.UID == "" {
		return problem(foreignParent, nil)
	}
	orphan := metav1.DeletePropagationOrphan
	err := c.config.Client.Delete(ctx, object.Name, metav1.DeleteOptions{PropagationPolicy: &orphan, Preconditions: &metav1.Preconditions{UID: &object.UID}})
	if apierrors.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		return problem(nativeFailure, err)
	}

	id, entry, _ := owned(object)
	return c.config.ForgetAdmission(ctx, id, entry)
}
