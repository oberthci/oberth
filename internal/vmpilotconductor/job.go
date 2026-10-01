// Package vmpilotconductor implements the inactive, operator-owned conductor
// Job lifecycle. It does not attach streams, execute guest code, or authorize
// suite success. Scheduler dispatch remains unavailable.
package vmpilotconductor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"

	"github.com/oberthci/oberth/internal/vmrunner"
)

const (
	// Executable is the required path in the independently verified producer
	// image. This package never downloads or substitutes that producer.
	Executable    = "/usr/local/bin/beacon-conductor"
	identityLabel = "oberth.ci/pilot-conductor"
)

var (
	jobs = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
	pods = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
)

type Config struct {
	ServiceAccount string
	RequestTimeout time.Duration
}

type Backend struct {
	client  dynamic.Interface
	journal vmrunner.PilotJournal
	config  Config
}

func New(client dynamic.Interface, journal vmrunner.PilotJournal, config Config) (*Backend, error) {
	if client == nil || journal == nil || len(validation.IsDNS1123Subdomain(config.ServiceAccount)) != 0 {
		return nil, errors.New("conductor: client, journal and operator ServiceAccount are required")
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 10 * time.Second
	}
	if config.RequestTimeout < time.Millisecond || config.RequestTimeout > time.Minute {
		return nil, errors.New("conductor: API timeout is outside its bound")
	}
	return &Backend{client: client, journal: journal, config: config}, nil
}

func ptr[T any](value T) *T { return &value }

func identity(value any) string {
	body, _ := json.Marshal(value)
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func (backend *Backend) job(plan vmrunner.PilotPlan) (*batchv1.Job, vmrunner.ResourceIntent, error) {
	if err := vmrunner.ValidatePilotPlan(plan); err != nil {
		return nil, vmrunner.ResourceIntent{}, err
	}
	seal := vmrunner.PilotIdentity(plan)
	labels := map[string]string{identityLabel: seal[:40]}
	job := &batchv1.Job{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: "pilot-conductor-" + seal[:32], Namespace: plan.ConductorNamespace, Labels: labels},
		Spec: batchv1.JobSpec{Parallelism: ptr[int32](1), Completions: ptr[int32](1), BackoffLimit: ptr[int32](0),
			ActiveDeadlineSeconds: ptr[int64](int64(plan.Spec.Deadline / time.Second)), Suspend: ptr(false),
			CompletionMode: ptr(batchv1.NonIndexedCompletion), ManualSelector: ptr(false), PodReplacementPolicy: ptr(batchv1.Failed),
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
				ServiceAccountName: backend.config.ServiceAccount, AutomountServiceAccountToken: ptr(false), EnableServiceLinks: ptr(false),
				RestartPolicy: corev1.RestartPolicyNever, DNSPolicy: corev1.DNSClusterFirst, SchedulerName: corev1.DefaultSchedulerName,
				TerminationGracePeriodSeconds: ptr[int64](1), NodeSelector: map[string]string{"kubernetes.io/arch": "amd64", "kubernetes.io/os": "linux"},
				SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr(true), RunAsUser: ptr[int64](65534), RunAsGroup: ptr[int64](65534), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				Containers: []corev1.Container{{Name: "conductor", Image: plan.ConductorImageRef, ImagePullPolicy: corev1.PullIfNotPresent,
					Command: []string{Executable}, Stdin: true, StdinOnce: true,
					TerminationMessagePath: corev1.TerminationMessagePathDefault, TerminationMessagePolicy: corev1.TerminationMessageReadFile,
					SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
					Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("256Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("256Mi")}},
				}},
			}},
		},
	}
	intent := vmrunner.ResourceIntent{Key: vmrunner.ConductorResource, Kind: "Job", Namespace: job.Namespace, Name: job.Name, SpecIdentity: identity(job.Spec)}
	return job, intent, nil
}

func (backend *Backend) execution(ctx context.Context, plan vmrunner.PilotPlan) (vmrunner.PilotExecution, error) {
	if err := vmrunner.ValidatePilotPlan(plan); err != nil {
		return vmrunner.PilotExecution{}, err
	}
	value, err := backend.journal.PilotExecution(ctx, plan.Spec.RunID)
	if err != nil {
		return value, err
	}
	if vmrunner.PilotIdentity(value.Plan) != vmrunner.PilotIdentity(plan) {
		return value, errors.New("conductor: plan differs from durable admission")
	}
	return value, nil
}

func (backend *Backend) resource(ctx context.Context, plan vmrunner.PilotPlan, key string) (vmrunner.PilotResource, error) {
	resources, err := backend.journal.PilotResources(ctx, plan.Spec.RunID)
	if err != nil {
		return vmrunner.PilotResource{}, err
	}
	for _, value := range resources {
		if value.Intent.Key == key {
			return value, nil
		}
	}
	return vmrunner.PilotResource{}, errors.New("conductor: resource is not journalled")
}

func asUnstructured(value any) (*unstructured.Unstructured, error) {
	body, err := runtime.DefaultUnstructuredConverter.ToUnstructured(value)
	return &unstructured.Unstructured{Object: body}, err
}

// Create persists intent and wins the submit transition before its sole API
// create. Retrying an ambiguous submission only observes; it never creates again.
func (backend *Backend) Create(ctx context.Context, plan vmrunner.PilotPlan) (vmrunner.ResourceReceipt, error) {
	execution, err := backend.execution(ctx, plan)
	if err != nil {
		return vmrunner.ResourceReceipt{}, err
	}
	job, intent, err := backend.job(plan)
	if err != nil {
		return vmrunner.ResourceReceipt{}, err
	}
	if !execution.Submitted || execution.Cleaning || execution.Cleaned || execution.Failure != "" {
		return vmrunner.ResourceReceipt{}, errors.New("conductor: execution cannot create resources")
	}
	if err := backend.journal.AddPilotResource(ctx, plan.Spec.RunID, intent); err != nil {
		return vmrunner.ResourceReceipt{}, err
	}
	owned, err := backend.resource(ctx, plan, intent.Key)
	if err != nil {
		return vmrunner.ResourceReceipt{}, err
	}
	if owned.Intent != intent || owned.Rejected || owned.Cleaning || owned.Cleaned {
		return vmrunner.ResourceReceipt{}, errors.New("conductor: resource intent cannot be reused")
	}
	call, cancel := context.WithTimeout(ctx, backend.config.RequestTimeout)
	defer cancel()
	var observed *unstructured.Unstructured
	api := backend.client.Resource(jobs).Namespace(plan.ConductorNamespace)
	if owned.Submitted {
		observed, err = api.Get(call, intent.Name, metav1.GetOptions{})
	} else {
		object, conversionErr := asUnstructured(job)
		if conversionErr != nil {
			return vmrunner.ResourceReceipt{}, conversionErr
		}
		if err := call.Err(); err != nil {
			return vmrunner.ResourceReceipt{}, err
		}
		if err := backend.journal.SubmitPilotResource(call, plan.Spec.RunID, intent.Key); err != nil {
			return vmrunner.ResourceReceipt{}, err
		}
		observed, err = api.Create(call, object, metav1.CreateOptions{})
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsNotFound(err) {
			return vmrunner.ResourceReceipt{}, errors.Join(err, backend.journal.RejectPilotResource(ctx, plan.Spec.RunID, intent.Key))
		}
		if apierrors.IsAlreadyExists(err) {
			observed, err = api.Get(call, intent.Name, metav1.GetOptions{})
		}
	}
	if err != nil {
		return vmrunner.ResourceReceipt{}, errors.Join(vmrunner.ErrExecutionOutstanding, err)
	}
	receipt, err := backend.jobReceipt(plan, observed)
	if err != nil {
		return vmrunner.ResourceReceipt{}, err
	}
	if observationPredates(receipt.CreatedAt, execution.CreatedAt) {
		return vmrunner.ResourceReceipt{}, errors.New("conductor: Job predates durable execution")
	}
	if err := backend.journal.BindPilotResource(ctx, plan.Spec.RunID, intent.Key, receipt); err != nil {
		return vmrunner.ResourceReceipt{}, err
	}
	return receipt, nil
}

func mismatch(field string) error {
	return fmt.Errorf("conductor: observed %s differs from closed recipe", field)
}
