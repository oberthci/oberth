package vmpilotconductor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/oberthci/oberth/internal/vmrunner"
)

func decodeObject(object *unstructured.Unstructured, target any) error {
	if object == nil {
		return mismatch("missing object")
	}
	body, err := json.Marshal(object.Object)
	if err != nil || len(body) > 256<<10 {
		return mismatch("object size")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return mismatch("object schema")
	}
	return nil
}

func validMetadata(meta metav1.ObjectMeta, namespace string) bool {
	return meta.Namespace == namespace && len(validation.IsDNS1123Subdomain(meta.Name)) == 0 &&
		meta.UID != "" && len(meta.UID) <= 128 && !meta.CreationTimestamp.IsZero() && !meta.CreationTimestamp.After(time.Now())
}

// Kubernetes metav1.Time serializes to whole-second RFC3339. A zero-nanosecond
// observation cannot establish ordering within the admission second. Compare
// that lower bound at the reported precision without changing either timestamp.
// Precise observations retain the precise lower bound; upper bounds are intact.
func observationPredates(observed, lowerBound time.Time) bool {
	if observed.Nanosecond() == 0 {
		lowerBound = lowerBound.Truncate(time.Second)
	}
	return observed.Before(lowerBound)
}

// Foreground deletion adds a finalizer while an object is still present. It is
// an observation to retain, never permission to remove a finalizer or to mark
// cleanup complete. Only Pods may also carry the Job controller's tracking mark.
func validFinalizers(meta metav1.ObjectMeta, pod bool) bool {
	seen := make(map[string]bool, 2)
	for _, value := range meta.Finalizers {
		if seen[value] {
			return false
		}
		seen[value] = true
		switch value {
		case metav1.FinalizerDeleteDependents:
			if meta.DeletionTimestamp == nil {
				return false
			}
		case batchv1.JobTrackingFinalizer:
			if !pod {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func stripControllerLabels(labels map[string]string, job *batchv1.Job) bool {
	for key, expected := range map[string]string{
		"batch.kubernetes.io/controller-uid": string(job.UID), "controller-uid": string(job.UID),
		"batch.kubernetes.io/job-name": job.Name, "job-name": job.Name,
	} {
		if value, present := labels[key]; present {
			if value != expected {
				return false
			}
			delete(labels, key)
		}
	}
	return true
}

// Only reviewed inert API defaults are normalized. Security-, image-, command-
// and authority-bearing fields are compared against the complete closed spec.
//
//nolint:staticcheck // Kubernetes still returns its legacy ServiceAccount alias; validate it before normalization.
func normalizePodSpec(actual *corev1.PodSpec, expected corev1.PodSpec, scheduled bool) error {
	if actual.DeprecatedServiceAccount != "" {
		if actual.DeprecatedServiceAccount != expected.ServiceAccountName {
			return mismatch("ServiceAccount alias")
		}
		actual.DeprecatedServiceAccount = ""
	}
	if actual.PreemptionPolicy != nil && *actual.PreemptionPolicy == corev1.PreemptLowerPriority {
		actual.PreemptionPolicy = nil
	}
	if actual.Priority != nil && *actual.Priority == 0 {
		actual.Priority = nil
	}
	if scheduled && actual.NodeName != "" {
		if len(validation.IsDNS1123Subdomain(actual.NodeName)) != 0 {
			return mismatch("assigned node")
		}
		actual.NodeName = ""
	}
	defaultTolerations := []corev1.Toleration{
		{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr[int64](300)},
		{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr[int64](300)},
	}
	if scheduled && reflect.DeepEqual(actual.Tolerations, defaultTolerations) {
		actual.Tolerations = nil
	}
	// Quantities have equivalent textual representations after API admission.
	if len(actual.Containers) == len(expected.Containers) {
		for i := range actual.Containers {
			for name, wanted := range expected.Containers[i].Resources.Requests {
				if got, ok := actual.Containers[i].Resources.Requests[name]; ok && got.Cmp(wanted) == 0 {
					actual.Containers[i].Resources.Requests[name] = wanted
				}
			}
			for name, wanted := range expected.Containers[i].Resources.Limits {
				if got, ok := actual.Containers[i].Resources.Limits[name]; ok && got.Cmp(wanted) == 0 {
					actual.Containers[i].Resources.Limits[name] = wanted
				}
			}
		}
	}
	return nil
}

func (backend *Backend) jobReceipt(plan vmrunner.PilotPlan, object *unstructured.Unstructured) (vmrunner.ResourceReceipt, error) {
	var actual batchv1.Job
	if err := decodeObject(object, &actual); err != nil {
		return vmrunner.ResourceReceipt{}, err
	}
	expected, intent, err := backend.job(plan)
	if err != nil {
		return vmrunner.ResourceReceipt{}, err
	}
	if actual.APIVersion != "batch/v1" || actual.Kind != "Job" || !validMetadata(actual.ObjectMeta, plan.ConductorNamespace) || actual.Name != expected.Name ||
		len(actual.Annotations) != 0 || len(actual.OwnerReferences) != 0 || !validFinalizers(actual.ObjectMeta, false) || !reflect.DeepEqual(actual.Labels, expected.Labels) {
		return vmrunner.ResourceReceipt{}, mismatch("Job metadata")
	}
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"batch.kubernetes.io/controller-uid": string(actual.UID)}}
	if !reflect.DeepEqual(actual.Spec.Selector, selector) {
		return vmrunner.ResourceReceipt{}, mismatch("Job selector")
	}
	actual.Spec.Selector = nil
	if !stripControllerLabels(actual.Spec.Template.Labels, &actual) || !reflect.DeepEqual(actual.Spec.Template.ObjectMeta, expected.Spec.Template.ObjectMeta) {
		return vmrunner.ResourceReceipt{}, mismatch("Job template metadata")
	}
	if err := normalizePodSpec(&actual.Spec.Template.Spec, expected.Spec.Template.Spec, false); err != nil {
		return vmrunner.ResourceReceipt{}, err
	}
	if identity(actual.Spec) != intent.SpecIdentity {
		return vmrunner.ResourceReceipt{}, mismatch("Job spec")
	}
	return vmrunner.ResourceReceipt{UID: string(actual.UID), SpecIdentity: intent.SpecIdentity, CreatedAt: actual.CreationTimestamp.Time}, nil
}

// podReceipt records ownership, including an unsafe child that must be removed.
// It does not authorize execution; validatePod applies the closed recipe later.
func (backend *Backend) podReceipt(plan vmrunner.PilotPlan, job vmrunner.PilotResource, object *unstructured.Unstructured) (vmrunner.PilotOwnedPod, error) {
	var actual corev1.Pod
	if err := decodeObject(object, &actual); err != nil {
		return vmrunner.PilotOwnedPod{}, err
	}
	expected, _, err := backend.job(plan)
	if err != nil {
		return vmrunner.PilotOwnedPod{}, err
	}
	if actual.APIVersion != "v1" || actual.Kind != "Pod" || !validMetadata(actual.ObjectMeta, plan.ConductorNamespace) || actual.CreationTimestamp.Before(&metav1.Time{Time: job.Receipt.CreatedAt}) {
		return vmrunner.PilotOwnedPod{}, mismatch("Pod metadata")
	}
	if !ownedPod(object, job.Receipt.UID) {
		return vmrunner.PilotOwnedPod{}, mismatch("Pod owner")
	}
	if len(actual.OwnerReferences) != 1 || actual.OwnerReferences[0].Name != job.Intent.Name || actual.OwnerReferences[0].BlockOwnerDeletion == nil || !*actual.OwnerReferences[0].BlockOwnerDeletion {
		return vmrunner.PilotOwnedPod{}, mismatch("Pod owner reference")
	}
	if !validFinalizers(actual.ObjectMeta, true) {
		return vmrunner.PilotOwnedPod{}, mismatch("Pod finalizer")
	}
	if err := normalizePodSpec(&actual.Spec, expected.Spec.Template.Spec, true); err != nil {
		return vmrunner.PilotOwnedPod{}, err
	}
	return vmrunner.PilotOwnedPod{
		Intent:  vmrunner.ResourceIntent{Key: "conductor-pod-" + identity(string(actual.UID))[:24], Kind: "Pod", Namespace: actual.Namespace, Name: actual.Name, Parent: vmrunner.ConductorResource, SpecIdentity: identity(actual.Spec)},
		Receipt: vmrunner.ResourceReceipt{UID: string(actual.UID), OwnerUID: job.Receipt.UID, SpecIdentity: identity(actual.Spec), CreatedAt: actual.CreationTimestamp.Time},
	}, nil
}

func (backend *Backend) validatePod(plan vmrunner.PilotPlan, object *unstructured.Unstructured, jobUID string) (corev1.Pod, error) {
	var actual corev1.Pod
	if err := decodeObject(object, &actual); err != nil {
		return actual, err
	}
	expected, _, err := backend.job(plan)
	if err != nil {
		return actual, err
	}
	expected.UID = types.UID(jobUID)
	if len(actual.Annotations) != 0 || actual.DeletionTimestamp != nil || !stripControllerLabels(actual.Labels, expected) || !reflect.DeepEqual(actual.Labels, expected.Labels) {
		return actual, mismatch("Pod metadata")
	}
	nodeName := actual.Spec.NodeName
	if err := normalizePodSpec(&actual.Spec, expected.Spec.Template.Spec, true); err != nil {
		return actual, err
	}
	if identity(actual.Spec) != identity(expected.Spec.Template.Spec) {
		return actual, mismatch("Pod spec")
	}
	actual.Spec.NodeName = nodeName
	return actual, nil
}

func ownedPod(pod *unstructured.Unstructured, ownerUID string) bool {
	for _, owner := range pod.GetOwnerReferences() {
		if owner.APIVersion == "batch/v1" && owner.Kind == "Job" && string(owner.UID) == ownerUID && owner.Controller != nil && *owner.Controller {
			return true
		}
	}
	return false
}

// Listing is not label-filtered: labels can be changed while an owner-UID child
// still needs cleanup. An incomplete or oversized snapshot is never absence.
func (backend *Backend) children(ctx context.Context, plan vmrunner.PilotPlan, job vmrunner.PilotResource) ([]*unstructured.Unstructured, error) {
	call, cancel := context.WithTimeout(ctx, backend.config.RequestTimeout)
	defer cancel()
	list, err := backend.client.Resource(pods).Namespace(plan.ConductorNamespace).List(call, metav1.ListOptions{Limit: vmrunner.MaxPilotResources + 1})
	if err != nil {
		return nil, err
	}
	if list.GetContinue() != "" || len(list.Items) > vmrunner.MaxPilotResources {
		return nil, errors.New("conductor: incomplete or oversized Pod inventory")
	}
	var owned []*unstructured.Unstructured
	for i := range list.Items {
		if ownedPod(&list.Items[i], job.Receipt.UID) {
			owned = append(owned, &list.Items[i])
		}
	}
	return owned, nil
}
