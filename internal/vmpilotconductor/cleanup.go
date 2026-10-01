package vmpilotconductor

import (
	"context"
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oberthci/oberth/internal/vmrunner"
)

var _ vmrunner.PilotCleanupRuntime = (*Backend)(nil)

func sameReceipt(a, b vmrunner.ResourceReceipt) bool {
	return a.UID == b.UID && a.OwnerUID == b.OwnerUID && a.SpecIdentity == b.SpecIdentity && a.PodIP == b.PodIP && a.CreatedAt.Equal(b.CreatedAt)
}

func (backend *Backend) owned(ctx context.Context, plan vmrunner.PilotPlan, given vmrunner.PilotResource) (vmrunner.PilotResource, schema.GroupVersionResource, error) {
	var kind schema.GroupVersionResource
	if _, err := backend.execution(ctx, plan); err != nil {
		return given, kind, err
	}
	actual, err := backend.resource(ctx, plan, given.Intent.Key)
	if err != nil {
		return given, kind, err
	}
	if actual.Intent != given.Intent || !sameReceipt(actual.Receipt, given.Receipt) || !actual.Submitted || actual.Rejected {
		return actual, kind, mismatch("durable resource ownership")
	}
	switch {
	case actual.Intent.Kind == "Job" && actual.Intent.Key == vmrunner.ConductorResource:
		kind = jobs
	case actual.Intent.Kind == "Pod" && actual.Intent.Parent == vmrunner.ConductorResource:
		kind = pods
	default:
		return actual, kind, errors.New("conductor: cleanup resource belongs to another backend")
	}
	return actual, kind, nil
}

// Observe checks the recorded UID and complete actual spec. Child inventory is
// owner-UID based even after the Job is absent; labels cannot hide survivors.
func (backend *Backend) Observe(ctx context.Context, plan vmrunner.PilotPlan, given vmrunner.PilotResource) (vmrunner.PilotResourceObservation, error) {
	var result vmrunner.PilotResourceObservation
	owned, kind, err := backend.owned(ctx, plan, given)
	if err != nil {
		return result, err
	}
	call, cancel := context.WithTimeout(ctx, backend.config.RequestTimeout)
	defer cancel()
	object, err := backend.client.Resource(kind).Namespace(owned.Intent.Namespace).Get(call, owned.Intent.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		result.Absent = true
	} else if err != nil {
		return result, err
	}
	if !result.Absent {
		if owned.Receipt.UID != "" && string(object.GetUID()) != owned.Receipt.UID {
			return result, mismatch("cleanup UID")
		}
		if kind == jobs {
			result.Receipt, err = backend.jobReceipt(plan, object)
		} else {
			parent, parentErr := backend.resource(ctx, plan, vmrunner.ConductorResource)
			if parentErr != nil {
				return result, parentErr
			}
			pod, podErr := backend.podReceipt(plan, parent, object)
			err = podErr
			result.Receipt = pod.Receipt
			if err == nil && pod.Intent != owned.Intent {
				err = mismatch("cleanup Pod intent")
			}
		}
		if err != nil {
			return result, err
		}
		if owned.Receipt.UID != "" && !sameReceipt(result.Receipt, owned.Receipt) {
			return result, mismatch("cleanup receipt")
		}
		value, err := backend.execution(ctx, plan)
		if err != nil {
			return result, err
		}
		if observationPredates(result.Receipt.CreatedAt, value.CreatedAt) {
			return result, mismatch("cleanup creation time")
		}
		owned.Receipt = result.Receipt
	}
	if kind == jobs && owned.Receipt.UID != "" {
		children, err := backend.children(ctx, plan, owned)
		if err != nil {
			return result, err
		}
		for _, child := range children {
			pod, err := backend.podReceipt(plan, owned, child)
			if err != nil {
				return result, err
			}
			result.Pods = append(result.Pods, pod)
		}
	}
	return result, nil
}

// Delete acknowledges only a UID-preconditioned request. The reconciler must
// observe absence afterward before releasing capacity. No finalizer is removed.
func (backend *Backend) Delete(ctx context.Context, plan vmrunner.PilotPlan, given vmrunner.PilotResource) error {
	owned, kind, err := backend.owned(ctx, plan, given)
	if err != nil {
		return err
	}
	execution, err := backend.execution(ctx, plan)
	if err != nil {
		return err
	}
	if !execution.Cleaning || !owned.Cleaning || owned.Cleaned || owned.Receipt.UID == "" {
		return errors.New("conductor: deletion requires a bound cleanup obligation")
	}
	observation, err := backend.Observe(ctx, plan, owned)
	if err != nil {
		return err
	}
	if observation.Absent {
		return nil
	}
	call, cancel := context.WithTimeout(ctx, backend.config.RequestTimeout)
	defer cancel()
	uid := types.UID(owned.Receipt.UID)
	err = backend.client.Resource(kind).Namespace(owned.Intent.Namespace).Delete(call, owned.Intent.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: ptr(metav1.DeletePropagationForeground)})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
