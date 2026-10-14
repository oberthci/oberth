package argojob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func (controller *Controller) trustedKVMWorkflow(ctx context.Context, request Request) (*wfv1.Workflow, *wfv1.Template, error) {
	if request.TrustedKVM == nil {
		return nil, nil, errors.New("argojob: missing trusted KVM intent")
	}
	request, err := controller.PrepareNonroot(ctx, request)
	if err != nil {
		return nil, nil, err
	}
	expected, err := Build(controller.config, request)
	if err != nil {
		return nil, nil, err
	}
	actual, err := controller.workflows.Get(ctx, request.Name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	if actual.UID == "" || actual.DeletionTimestamp != nil || actual.Name != expected.Name || actual.Namespace != expected.Namespace || actual.Annotations[runIDAnnotation] != request.RunID || actual.Annotations[trustedKVMAnnotation] != expected.Annotations[trustedKVMAnnotation] || actual.Annotations[nonrootProfileAnnotation] != expected.Annotations[nonrootProfileAnnotation] || !reflect.DeepEqual(actual.Spec, expected.Spec) {
		return nil, nil, errors.New("argojob: trusted Workflow differs from admitted final server spec")
	}
	for i := range expected.Spec.Templates {
		if expected.Spec.Templates[i].Name == trustedKVMTemplate {
			return actual, &expected.Spec.Templates[i], nil
		}
	}
	return nil, nil, errors.New("argojob: missing trusted leaf")
}

// TrustedKVMWorkflowUID binds only an API object whose actual spec matches the
// rebuilt server submission, never an identity annotation supplied as evidence.
func (controller *Controller) TrustedKVMWorkflowUID(ctx context.Context, request Request) (string, error) {
	workflow, _, err := controller.trustedKVMWorkflow(ctx, request)
	if err != nil {
		return "", err
	}
	return string(workflow.UID), nil
}

// TrustedKVMReceipt uses current kubelet status from one exactly owned Pod.
// Workflow success only determines readiness; it never substitutes for evidence.
func (controller *Controller) TrustedKVMReceipt(ctx context.Context, request Request, workflowUID string) (vmrunner.KVMReceipt, error) {
	workflow, leaf, err := controller.trustedKVMWorkflow(ctx, request)
	if err != nil {
		return vmrunner.KVMReceipt{}, err
	}
	if controller.kube == nil || workflowUID == "" || string(workflow.UID) != workflowUID || workflow.Status.Phase != wfv1.WorkflowSucceeded {
		return vmrunner.KVMReceipt{}, errors.New("argojob: trusted Workflow not exactly bound and successful")
	}
	var selected *wfv1.NodeStatus
	for _, node := range workflow.Status.Nodes {
		if node.Type == wfv1.NodeTypePod && node.TemplateName == trustedKVMTemplate {
			if selected != nil {
				return vmrunner.KVMReceipt{}, errors.New("argojob: multiple trusted Pod attempts")
			}
			copy := node
			selected = &copy
		}
	}
	if selected == nil || selected.Phase != wfv1.NodeSucceeded || selected.ID == "" || selected.Name != workflow.Name+"[1].trusted-tests" || selected.BoundaryID != workflow.Status.Nodes[workflow.Name].ID {
		return vmrunner.KVMReceipt{}, errors.New("argojob: trusted leaf node linkage differs from server wrapper")
	}
	name := PodName(workflow, *selected)
	pod, err := controller.kube.CoreV1().Pods(workflow.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return vmrunner.KVMReceipt{}, err
	}
	if err := verifyTrustedKVMPod(workflow, leaf, pod, selected, controller.config); err != nil {
		return vmrunner.KVMReceipt{}, err
	}
	// The profile forbids background producers; observe every Workflow-owned
	// producer terminal before accepting the private artifact snapshot report.
	pods, err := controller.kube.CoreV1().Pods(workflow.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return vmrunner.KVMReceipt{}, err
	}
	for i := range pods.Items {
		other := &pods.Items[i]
		if !ownedByWorkflow(other, workflow.Name, workflow.UID) {
			for _, owner := range other.OwnerReferences {
				if owner.UID == workflow.UID {
					return vmrunner.KVMReceipt{}, errors.New("argojob: ambiguous producer ownership")
				}
			}
			continue
		}
		if other.UID == pod.UID {
			continue
		}
		if other.Name == executorIdentityPodName(workflow.UID) && len(other.Spec.SchedulingGates) == 1 && other.Spec.SchedulingGates[0].Name == "oberth.ci/executor-identity" && other.Spec.NodeName == "" {
			continue
		}
		if other.Status.Phase != corev1.PodSucceeded && other.Status.Phase != corev1.PodFailed {
			return vmrunner.KVMReceipt{}, errors.New("argojob: a producer remains active during trusted verdict")
		}
	}
	var status *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == "main" {
			if status != nil {
				return vmrunner.KVMReceipt{}, errors.New("argojob: duplicate harness status")
			}
			status = &pod.Status.ContainerStatuses[i]
		}
	}
	if status == nil || status.State.Terminated == nil {
		return vmrunner.KVMReceipt{}, errors.New("argojob: harness has no kubelet termination")
	}
	intent := *request.TrustedKVM
	binding := vmrunner.KVMTerminationBinding{Namespace: workflow.Namespace, WorkflowName: workflow.Name, WorkflowUID: workflowUID, PodName: pod.Name, PodUID: string(pod.UID), ContainerID: status.ContainerID, HarnessImageRef: intent.Recipe.HarnessImage, AdmittedAt: intent.AdmittedAt, Deadline: intent.Deadline}
	if err := vmrunner.VerifyKVMTermination(binding, pod); err != nil {
		return vmrunner.KVMReceipt{}, err
	}
	message := []byte(status.State.Terminated.Message)
	if len(message) == 0 || len(message) > 4096 {
		return vmrunner.KVMReceipt{}, errors.New("argojob: harness termination report missing or too large")
	}
	var report vmrunner.KVMReport
	decoder := json.NewDecoder(bytes.NewReader(message))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return vmrunner.KVMReceipt{}, errors.New("argojob: invalid harness termination report")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return vmrunner.KVMReceipt{}, errors.New("argojob: trailing harness report data")
	}
	// Exact canonical encoding rejects duplicate JSON keys and alternate records.
	canonical, _ := json.Marshal(report)
	if !bytes.Equal(bytes.TrimSpace(message), canonical) {
		return vmrunner.KVMReceipt{}, errors.New("argojob: noncanonical harness report")
	}
	terminated := status.State.Terminated
	receipt := vmrunner.KVMReceipt{IntentDigest: vmrunner.KVMIntentDigest(intent), WorkflowUID: workflowUID, Binding: binding, Report: report, FinishedAt: terminated.FinishedAt.Time,
		Termination: vmrunner.KVMTerminationRecord{ImageID: status.ImageID, ContainerID: status.ContainerID, PodUID: string(pod.UID), WorkflowUID: workflowUID, ExitCode: terminated.ExitCode, Signal: terminated.Signal, RestartCount: status.RestartCount, StartedAt: terminated.StartedAt.Time, FinishedAt: terminated.FinishedAt.Time}}
	if err := receipt.Validate(intent); err != nil {
		return vmrunner.KVMReceipt{}, err
	}
	return receipt, nil
}

func ownedByWorkflow(pod *corev1.Pod, name string, uid types.UID) bool {
	if len(pod.OwnerReferences) != 1 {
		return false
	}
	owner := pod.OwnerReferences[0]
	return owner.Name == name && owner.UID == uid && owner.Kind == "Workflow" && owner.APIVersion == "argoproj.io/v1alpha1" && owner.Controller != nil && *owner.Controller
}

func verifyTrustedKVMPod(workflow *wfv1.Workflow, leaf *wfv1.Template, pod *corev1.Pod, node *wfv1.NodeStatus, config Config) error {
	invalid := func(reason string) error { return fmt.Errorf("argojob: trusted Pod attestation failed: %s", reason) }
	if pod.Namespace != workflow.Namespace || pod.UID == "" || pod.Name != PodName(workflow, *node) || !ownedByWorkflow(pod, workflow.Name, workflow.UID) || pod.Annotations["workflows.argoproj.io/node-id"] != node.ID || pod.Annotations["workflows.argoproj.io/node-name"] != node.Name {
		return invalid("ownership and node linkage")
	}
	if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC || (pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace) || pod.Spec.ServiceAccountName != config.PipelineServiceAccount || pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken || pod.Spec.RestartPolicy != corev1.RestartPolicyNever || len(pod.Spec.EphemeralContainers) != 0 || len(pod.Spec.Containers) != 2 || len(pod.Spec.InitContainers) != 2 || !reflect.DeepEqual(pod.Spec.SecurityContext, leaf.SecurityContext) || !reflect.DeepEqual(pod.Spec.NodeSelector, leaf.NodeSelector) || pod.Spec.NodeName == "" {
		return invalid("Pod isolation")
	}
	var main, wait *corev1.Container
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		switch c.Name {
		case "main":
			if main != nil {
				return invalid("duplicate main")
			}
			main = c
		case "wait":
			if wait != nil {
				return invalid("duplicate executor")
			}
			wait = c
		default:
			return invalid("unexpected helper")
		}
	}
	if main == nil || wait == nil {
		return invalid("missing containers")
	}
	expected := leaf.Container.DeepCopy()
	expected.Name = "main"
	// Argo 4.0.8 wraps the exact reviewed command with its pinned emissary.
	expected.Command = append([]string{"/var/run/argo/argoexec", "emissary", "--loglevel", "info", "--log-format", "text", "--gloglevel", "0", "--"}, expected.Command...)
	observed := main.DeepCopy()
	if len(observed.EnvFrom) != 0 {
		return invalid("ambient main environment")
	}
	var env []corev1.EnvVar
	seen := map[string]bool{}
	for _, v := range observed.Env {
		if seen[v.Name] {
			return invalid("duplicate main environment")
		}
		seen[v.Name] = true
		if strings.HasPrefix(v.Name, "ARGO_") {
			switch v.Name {
			case "ARGO_CONTAINER_NAME":
				if v.Value != "main" || v.ValueFrom != nil {
					return invalid("Argo container binding")
				}
			case "ARGO_NODE_ID":
				if v.Value != node.ID || v.ValueFrom != nil {
					return invalid("Argo node binding")
				}
			case "ARGO_INCLUDE_SCRIPT_OUTPUT":
				if v.Value != "false" || v.ValueFrom != nil {
					return invalid("script capture")
				}
			case "ARGO_DEADLINE", "ARGO_PROGRESS_FILE", "ARGO_PROGRESS_PATCH_TICK_DURATION", "ARGO_PROGRESS_FILE_TICK_DURATION", "ARGO_TERMINATION_GRACE_PERIOD_SECONDS":
				if !trustedArgoRuntimeValue(v, workflow, pod) {
					return invalid("Argo runtime environment value")
				}
			default:
				return invalid("unexpected Argo environment")
			}
			continue
		}
		env = append(env, v)
	}
	for _, required := range []string{"ARGO_CONTAINER_NAME", "ARGO_NODE_ID", "ARGO_INCLUDE_SCRIPT_OUTPUT", "ARGO_DEADLINE"} {
		if !seen[required] {
			return invalid("missing Argo main binding")
		}
	}
	observed.Env = env
	var mounts []corev1.VolumeMount
	argoMount := corev1.VolumeMount{Name: "var-run-argo", MountPath: "/var/run/argo"}
	argoSeen := false
	for _, m := range observed.VolumeMounts {
		if m.Name == "var-run-argo" {
			if argoSeen || !reflect.DeepEqual(m, argoMount) {
				return invalid("executor binary mount")
			}
			argoSeen = true
			continue
		}
		mounts = append(mounts, m)
	}
	if !argoSeen {
		return invalid("missing executor binary mount")
	}
	observed.VolumeMounts = mounts
	// API defaults do not alter executable authority.
	observed.ImagePullPolicy = ""
	observed.TerminationMessagePath = "/dev/termination-log"
	observed.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	if main.TerminationMessagePath != "/dev/termination-log" || main.TerminationMessagePolicy != corev1.TerminationMessageReadFile || !reflect.DeepEqual(observed, expected) {
		return invalid("main executable, environment, mounts, resources or security")
	}
	if err := verifyTrustedKVMHelpers(pod, wait, config, workflow, leaf); err != nil {
		return err
	}
	allowed := map[string]corev1.Volume{}
	for _, v := range leaf.Volumes {
		allowed[v.Name] = v
	}
	for _, name := range []string{"var-run-argo", "tmp-dir-argo"} {
		allowed[name] = corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	}
	// Projected executor token is memory-only and never accessible to main.
	allowed[executorTokenVolume] = corev1.Volume{Name: executorTokenVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: ptr.To(resource.MustParse("1Mi"))}}}
	for _, v := range pod.Spec.Volumes {
		want, ok := allowed[v.Name]
		if !ok || !reflect.DeepEqual(v, want) {
			return invalid("volume sources")
		}
		delete(allowed, v.Name)
	}
	if len(allowed) != 0 {
		return invalid("missing private volumes")
	}
	return nil
}

func verifyTrustedKVMHelpers(pod *corev1.Pod, wait *corev1.Container, config Config, workflow *wfv1.Workflow, leaf *wfv1.Template) error {
	invalid := func(reason string) error {
		return fmt.Errorf("argojob: trusted executor attestation failed: %s", reason)
	}
	if pod.Spec.InitContainers[0].Name != executorTokenWaiter || pod.Spec.InitContainers[1].Name != "init" {
		return invalid("initializer order")
	}
	waiter := pod.Spec.InitContainers[0].DeepCopy()
	expectedWaiter := corev1.Container{Name: executorTokenWaiter, Image: config.SourceSeedImage, Command: []string{"sh", "-c", executorTokenWaitCommand}, SecurityContext: nonrootContainerSecurity(0), VolumeMounts: []corev1.VolumeMount{{Name: executorTokenVolume, MountPath: executorTokenDir}}}
	waiter.ImagePullPolicy = ""
	waiter.TerminationMessagePath = ""
	waiter.TerminationMessagePolicy = ""
	if !reflect.DeepEqual(*waiter, expectedWaiter) {
		return invalid("token waiter")
	}
	for _, c := range []*corev1.Container{&pod.Spec.InitContainers[1], wait} {
		if c.Image != argoworkflow.NonrootExecutorImage || len(c.EnvFrom) != 0 || len(c.VolumeDevices) != 0 || c.Lifecycle != nil || c.LivenessProbe != nil || c.StartupProbe != nil || c.ReadinessProbe != nil {
			return invalid("executor image or runtime hooks")
		}
		expected := []string{"argoexec", c.Name, "--loglevel", "info", "--log-format", "text", "--gloglevel", "0"}
		if !reflect.DeepEqual(c.Command, expected) || (len(c.Args) != 0 && !reflect.DeepEqual(c.Args, []string{"--gloglevel", "0"})) {
			return invalid("executor command")
		}
		if !reflect.DeepEqual(c.SecurityContext, nonrootContainerSecurity(0)) {
			return invalid("executor security")
		}

		for _, m := range c.VolumeMounts {
			valid := (m.Name == executorTokenVolume && m.MountPath == executorTokenDir && m.ReadOnly && m.SubPath == "") || (m.Name == "var-run-argo" && m.MountPath == "/var/run/argo" && m.SubPath == "") || (m.Name == "tmp-dir-argo" && m.MountPath == "/tmp" && m.SubPath == "0")
			if !valid || m.SubPathExpr != "" || m.MountPropagation != nil {
				return invalid("helper can reach private state or input artifacts")
			}
		}
		seen := map[string]bool{}
		for _, e := range c.Env {
			if seen[e.Name] {
				return invalid("duplicate executor environment")
			}
			seen[e.Name] = true
			switch e.Name {
			case "ARGO_POD_NAME", "ARGO_POD_UID":
				field := "metadata.name"
				if e.Name == "ARGO_POD_UID" {
					field = "metadata.uid"
				}
				expected := corev1.EnvVar{Name: e.Name, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: field}}}
				if !reflect.DeepEqual(e, expected) {
					return invalid("executor Pod identity")
				}
			case "ARGO_WORKFLOW_NAME":
				if e.Value != pod.OwnerReferences[0].Name || e.ValueFrom != nil {
					return invalid("executor Workflow name")
				}
			case "ARGO_WORKFLOW_UID":
				if e.Value != string(pod.OwnerReferences[0].UID) || e.ValueFrom != nil {
					return invalid("executor Workflow UID")
				}
			case "ARGO_NODE_ID":
				if e.Value != pod.Annotations["workflows.argoproj.io/node-id"] || e.ValueFrom != nil {
					return invalid("executor node identity")
				}
			case "ARGO_CONTAINER_NAME":
				if e.Value != c.Name || e.ValueFrom != nil {
					return invalid("executor container identity")
				}
			case "ARGO_INCLUDE_SCRIPT_OUTPUT":
				if e.Value != "false" || e.ValueFrom != nil {
					return invalid("executor script capture")
				}
			case "ARGO_TEMPLATE":
				if c.Name != "init" || e.ValueFrom != nil {
					return invalid("executor template")
				}
				var template wfv1.Template
				expectedTemplate := leaf.DeepCopy()
				if workflow.Spec.PodMetadata != nil {
					expectedTemplate.Metadata.Labels = workflow.Spec.PodMetadata.Labels
				}
				if json.Unmarshal([]byte(e.Value), &template) != nil || !reflect.DeepEqual(template, *expectedTemplate) {
					return invalid("executor template data")
				}
				// An init template with artifacts or archive directives can run independent
				// I/O before the pinned main; the trusted leaf has neither.
				if len(template.Inputs.Artifacts) != 0 || len(template.Outputs.Artifacts) != 0 || template.ArchiveLocation != nil {
					return invalid("executor template I/O")
				}
			case "ARGO_ARTIFACT_PLUGIN_NAMES":
				if e.Value != "" || e.ValueFrom != nil {
					return invalid("executor artifact plugin")
				}
			case "ARGO_DEADLINE", "ARGO_PROGRESS_FILE", "ARGO_PROGRESS_PATCH_TICK_DURATION", "ARGO_PROGRESS_FILE_TICK_DURATION", "ARGO_TERMINATION_GRACE_PERIOD_SECONDS":
				if !trustedArgoRuntimeValue(e, workflow, pod) {
					return invalid("executor runtime environment value")
				}
			default:
				return invalid("unexpected executor environment")
			}
		}
	}
	return nil
}

// CleanupTrustedKVM only destroys exact owned identities. It returns success
// after observing the Workflow, every owned Pod and the source claim absent.
func (controller *Controller) CleanupTrustedKVM(ctx context.Context, name, runID, expectedUID string) error {
	if controller.kube == nil || name == "" || runID == "" || expectedUID == "" {
		return errors.New("argojob: cleanup requires a bound trusted Workflow UID")
	}
	uid := types.UID(expectedUID)
	ticker := time.NewTicker(controller.interval)
	defer ticker.Stop()
	for {
		absent, err := controller.cleanTrustedKVMOnce(ctx, name, runID, uid)
		if err != nil {
			return err
		}
		if absent {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (controller *Controller) cleanTrustedKVMOnce(ctx context.Context, name, runID string, uid types.UID) (bool, error) {
	wf, err := controller.workflows.Get(ctx, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	workflowAbsent := apierrors.IsNotFound(err)
	if !workflowAbsent {
		if wf.UID != uid || wf.Name != name || wf.Namespace != controller.config.Namespace || wf.Annotations[runIDAnnotation] != runID || wf.Annotations[trustedKVMAnnotation] == "" {
			return false, errors.New("argojob: refusing foreign trusted Workflow cleanup")
		}
		if wf.DeletionTimestamp == nil {
			err = controller.workflows.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: ptr.To(metav1.DeletePropagationForeground)})
			if err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
	}
	pods, err := controller.kube.CoreV1().Pods(controller.config.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	owned := 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !ownedByWorkflow(pod, name, uid) {
			for _, owner := range pod.OwnerReferences {
				if owner.UID == uid {
					return false, errors.New("argojob: Pod retains ambiguous trusted Workflow ownership")
				}
			}
			continue
		}
		owned++
		if pod.UID == "" {
			return false, errors.New("argojob: owned Pod lacks immutable UID")
		}
		if pod.DeletionTimestamp == nil {
			err = controller.kube.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}})
			if err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
	}
	if !workflowAbsent || owned != 0 {
		return false, nil
	}
	for _, pod := range pods.Items {
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == sourceClaimName(name) {
				return false, errors.New("argojob: source claim still mounted by a Pod")
			}
		}
	}
	claims := controller.kube.CoreV1().PersistentVolumeClaims(controller.config.Namespace)
	claim, err := claims.Get(ctx, sourceClaimName(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if claim.UID == "" || len(claim.OwnerReferences) != 1 || claim.OwnerReferences[0].UID != uid || claim.OwnerReferences[0].Name != name || claim.OwnerReferences[0].Kind != "Workflow" || claim.OwnerReferences[0].APIVersion != "argoproj.io/v1alpha1" {
		return false, errors.New("argojob: source claim is not bound to exact trusted Workflow")
	}
	if claim.DeletionTimestamp == nil {
		err = claims.Delete(ctx, claim.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &claim.UID}})
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return false, nil
}

// ErrTrustedKVMCreateRefused proves no Workflow was admitted and all seeding
// auxiliaries are absent. It covers pre-POST failures and explicit API admission
// rejections, never ambiguous timeout, transport or server failures.
var ErrTrustedKVMCreateRefused = errors.New("argojob: trusted Workflow create definitively refused")

// ErrTrustedKVMWorkflowPresent directs recovery to immutable UID/spec attestation.
var ErrTrustedKVMWorkflowPresent = errors.New("argojob: trusted Workflow exists")

// trustedCreateAbsenceWindow covers the maximum Kubernetes API request timeout.
// A restart cannot recover the in-flight POST's start time, so absence must be
// freshly observed for this entire interval before releasing its reservation.
const trustedCreateAbsenceWindow = 60 * time.Second

func (controller *Controller) ConfirmTrustedKVMAbsent(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, trustedCreateAbsenceWindow+30*time.Second)
	defer cancel()
	return controller.observeTrustedKVMAbsence(ctx, name, time.Now, func(ctx context.Context) error {
		timer := time.NewTimer(controller.interval)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	})
}

// now/wait form a private deterministic time seam; production always uses a
// monotonic wall clock and real timers. Every observation must succeed.
func (controller *Controller) observeTrustedKVMAbsence(ctx context.Context, name string, now func() time.Time, wait func(context.Context) error) error {
	var since time.Time
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		absent, err := controller.trustedKVMAbsentOnce(ctx, name)
		if err != nil {
			return err
		}
		if !absent {
			since = time.Time{}
		} else {
			observed := now()
			if since.IsZero() {
				since = observed
			}
			if observed.Sub(since) >= trustedCreateAbsenceWindow {
				return nil
			}
		}
		if err := wait(ctx); err != nil {
			return err
		}
	}
}

func (controller *Controller) confirmTrustedKVMNoCreate(ctx context.Context, name string) error {
	ticker := time.NewTicker(controller.interval)
	defer ticker.Stop()
	for {
		absent, err := controller.trustedKVMAbsentOnce(ctx, name)
		if err != nil {
			return err
		}
		if absent {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (controller *Controller) trustedKVMAbsentOnce(ctx context.Context, name string) (bool, error) {
	if controller.kube == nil || name == "" {
		return false, errors.New("argojob: cannot establish pre-create cleanup")
	}
	if _, err := controller.workflows.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return false, ErrTrustedKVMWorkflowPresent
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	// Never delete an unbound object based on labels or a name collision.
	claimName := sourceClaimName(name)
	pods, err := controller.kube.CoreV1().Pods(controller.config.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for _, pod := range pods.Items {
		if pod.Name == claimName {
			return false, nil
		}
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claimName {
				return false, nil
			}
		}
	}
	_, err = controller.kube.CoreV1().PersistentVolumeClaims(controller.config.Namespace).Get(ctx, claimName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	return false, err
}

func trustedArgoRuntimeValue(env corev1.EnvVar, workflow *wfv1.Workflow, pod *corev1.Pod) bool {
	if env.ValueFrom != nil {
		return false
	}
	switch env.Name {
	case "ARGO_PROGRESS_FILE":
		return env.Value == "/var/run/argo/progress"
	case "ARGO_PROGRESS_PATCH_TICK_DURATION":
		return env.Value == "1m0s"
	case "ARGO_PROGRESS_FILE_TICK_DURATION":
		return env.Value == "3s"
	case "ARGO_TERMINATION_GRACE_PERIOD_SECONDS":
		return pod.Spec.TerminationGracePeriodSeconds != nil && env.Value == strconv.FormatInt(*pod.Spec.TerminationGracePeriodSeconds, 10)
	case "ARGO_DEADLINE":
		return !workflow.Status.StartedAt.IsZero() && workflow.Spec.ActiveDeadlineSeconds != nil && env.Value == workflow.Status.StartedAt.Add(time.Duration(*workflow.Spec.ActiveDeadlineSeconds)*time.Second).UTC().Format(time.RFC3339)
	}
	return false
}

// TrustedKVMRefusal retains actual failed kubelet evidence after the same
// executable/ownership attestation. It never validates or grants a pass.
func (controller *Controller) TrustedKVMRefusal(ctx context.Context, request Request, workflowUID string) (*vmrunner.KVMTerminationRecord, error) {
	workflow, leaf, err := controller.trustedKVMWorkflow(ctx, request)
	if err != nil {
		return nil, err
	}
	if controller.kube == nil || workflowUID == "" || string(workflow.UID) != workflowUID {
		return nil, errors.New("argojob: refusal requires the exact bound Workflow")
	}
	var selected *wfv1.NodeStatus
	for _, node := range workflow.Status.Nodes {
		if node.Type == wfv1.NodeTypePod && node.TemplateName == trustedKVMTemplate {
			if selected != nil {
				return nil, errors.New("argojob: ambiguous trusted Pod refusal")
			}
			copy := node
			selected = &copy
		}
	}
	if selected == nil || selected.Name != workflow.Name+"[1].trusted-tests" || selected.BoundaryID != workflow.Status.Nodes[workflow.Name].ID {
		return nil, errors.New("argojob: trusted refusal has no linked Pod")
	}
	pod, err := controller.kube.CoreV1().Pods(workflow.Namespace).Get(ctx, PodName(workflow, *selected), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err := verifyTrustedKVMPod(workflow, leaf, pod, selected, controller.config); err != nil {
		return nil, err
	}
	var status *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == "main" {
			if status != nil {
				return nil, errors.New("argojob: duplicate harness refusal statuses")
			}
			status = &pod.Status.ContainerStatuses[i]
		}
	}
	if status == nil || status.State.Terminated == nil || status.ContainerID == "" || status.State.Terminated.ContainerID != status.ContainerID {
		return nil, errors.New("argojob: harness refusal has no current bound termination")
	}
	terminated := status.State.Terminated
	return &vmrunner.KVMTerminationRecord{ImageID: status.ImageID, ContainerID: status.ContainerID, PodUID: string(pod.UID), WorkflowUID: workflowUID, ExitCode: terminated.ExitCode, Signal: terminated.Signal, RestartCount: status.RestartCount, StartedAt: terminated.StartedAt.Time, FinishedAt: terminated.FinishedAt.Time}, nil
}
