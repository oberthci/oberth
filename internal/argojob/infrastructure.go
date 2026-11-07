package argojob

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
)

// Argo copies PodStatus.Message verbatim on eviction. This survives Pod GC.
// OOMKilled alone does not distinguish a step's memory limit from node pressure;
// only a kubelet eviction/pressure message establishes an infrastructure cause.
var kubeletEvictionMessage = regexp.MustCompile(`^(Usage of EmptyDir volume "[^"]+" exceeds the limit "[^"]+"\.|The node was low on resource: (ephemeral-storage|memory)\.|Pod ephemeral local storage usage exceeds the total limit of containers|Container [a-z0-9-]+ exceeded its local ephemeral storage limit)`)

// The kubelet prints the Pod total with resource.Quantity.String(): digits with
// a binary or decimal SI suffix, or a decimal exponent such as 1e9.
var podStorageTotalMessage = regexp.MustCompile(`^Pod ephemeral local storage usage exceeds the total limit of containers ([0-9]+(?:[.][0-9]+)?(?:[eE][+-]?[0-9]+|[A-Za-z]{0,2}))(?:[.\s]|$)`)

var containerStorageMessage = regexp.MustCompile(`^Container ([a-z0-9-]+) exceeded its local ephemeral storage limit`)

// stepStorageLimit describes the limit of a kubelet ephemeral-storage eviction
// that enforced the step's own declared budget, or returns "". Such a step
// exceeded what it declared: a rerun fails the same way, so it is a step
// resource failure, not infrastructure. Otherwise the eviction stays
// infrastructure and gets at most one auto-retry, which is rebuilt under the
// current policy.
//
// A container eviction qualifies when the named container is one of the step's
// own containers with a declared limit (stepStorageLimits); the kubelet only
// enforces a container limit that exists, but a server helper's limit is not
// the step's. A Pod-total eviction qualifies when the total includes no helper
// limit the step did not ask for (podTotalIsStepBudget).
func stepStorageLimit(workflow *wfv1.Workflow, node wfv1.NodeStatus, message string) string {
	if workflow == nil || node.Type != wfv1.NodeTypePod || (node.Phase != wfv1.NodeFailed && node.Phase != wfv1.NodeError) {
		return ""
	}
	message = strings.TrimPrefix(strings.TrimSpace(message), "Evicted: ")
	var template *wfv1.Template
	for i := range workflow.Spec.Templates {
		if node.TemplateName != "" && workflow.Spec.Templates[i].Name == node.TemplateName {
			template = &workflow.Spec.Templates[i]
			break
		}
	}
	if match := containerStorageMessage.FindStringSubmatch(message); match != nil {
		limit, ok := stepStorageLimits(workflow, template)[match[1]]
		if !ok {
			return ""
		}
		return fmt.Sprintf("container %s's own limit %s", match[1], limit.String())
	}
	match := podStorageTotalMessage.FindStringSubmatch(message)
	if match == nil {
		return ""
	}
	total, err := resource.ParseQuantity(match[1])
	if err != nil || total.Sign() <= 0 || !podTotalIsStepBudget(workflow, template) {
		return ""
	}
	return fmt.Sprintf("the total container limit %s (the step's limits plus Argo helper limits)", total.String())
}

// podTotalIsStepBudget reports whether a Pod ephemeral-storage total of
// template t is the step's own budget. v4 adds helper limits only when the step
// declares one, so any total is the step's. v3 did the same, except where its
// decision counted a limit Argo does not run (a template-default container-set
// member): then the total may be helper limits alone. Historical policies
// limited every helper; there only a named template with its own limit
// qualifies.
func podTotalIsStepBudget(workflow *wfv1.Workflow, t *wfv1.Template) bool {
	switch workflow.Annotations[helperStorageAnnotation] {
	case helperStoragePolicy:
		return true
	case stepLimitHelperStoragePolicy:
		return t == nil || !stepDeclaresStorageLimitV3(workflow, t) || stepDeclaresStorageLimit(workflow, t)
	}
	return t != nil && stepDeclaresStorageLimit(workflow, t)
}

func podStepStorageLimit(workflow *wfv1.Workflow, node wfv1.NodeStatus, pod *corev1.Pod) string {
	if !stepPodBound(workflow, node, pod) || pod.Status.Phase != corev1.PodFailed {
		return ""
	}
	return stepStorageLimit(workflow, node, pod.Status.Message)
}

func stepStorageLimitReason(step StepResult) string {
	return fmt.Sprintf("Step %s/%s exceeded its declared ephemeral-storage budget: the kubelet evicted its Pod at %s. Raise the step's ephemeral-storage limit in git.", step.Stage, step.Step, step.storageLimit)
}

func nodeInfrastructureReason(workflow *wfv1.Workflow, node wfv1.NodeStatus) string {
	if node.Type != wfv1.NodeTypePod || (node.Phase != wfv1.NodeFailed && node.Phase != wfv1.NodeError) {
		return ""
	}
	if stepStorageLimit(workflow, node, node.Message) != "" {
		return ""
	}
	message := strings.TrimSpace(node.Message)
	message = strings.TrimPrefix(message, "Evicted: ")
	if node.Message == "Evicted" {
		return "Evicted"
	}
	if kubeletEvictionMessage.MatchString(message) {
		return boundedKubeletMessage("Evicted: " + message)
	}
	return ""
}

func podInfrastructureReason(workflow *wfv1.Workflow, node wfv1.NodeStatus, pod *corev1.Pod) string {
	// Eviction may precede main startup or leave running container states, so
	// it needs the owned Pod binding, not a complete container diagnostic.
	if !stepPodBound(workflow, node, pod) || pod.Status.Phase != corev1.PodFailed || podStepStorageLimit(workflow, node, pod) != "" {
		return ""
	}
	message := strings.TrimSpace(pod.Status.Message)
	if pod.Status.Reason == "Evicted" || kubeletEvictionMessage.MatchString(message) {
		if message == "" {
			return "Evicted"
		}
		return boundedKubeletMessage("Evicted: " + message)
	}
	return ""
}

func boundedKubeletMessage(message string) string {
	message = strings.ToValidUTF8(message, "")
	if len(message) > 4096 {
		message = message[:4096]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}

func (completion *Completion) classifyInfrastructure() {
	if completion.Succeeded {
		return
	}
	reason, storage := "", ""
	failedStage, failedStep := "", ""
	for _, step := range completion.Steps {
		if step.Status == runprogress.StepPassed || step.Status == runprogress.StepSkipped || !terminal(step.Status) {
			continue
		}
		// A separate test failure must still open a CI-red issue and must not
		// become eligible for infrastructure retry.
		if step.AttemptAmbiguous || step.InfrastructureReason == "" {
			if failedStep == "" {
				failedStage, failedStep = step.Stage, step.Step
				if step.storageLimit != "" && !step.AttemptAmbiguous {
					storage = stepStorageLimitReason(step)
				}
			}
			continue
		}
		if reason == "" {
			reason = step.InfrastructureReason
		}
	}
	if failedStep != "" {
		// Owned Pod evidence can turn a node-level eviction into the step's
		// own declared storage budget; an earlier verdict must not survive.
		completion.Infrastructure = false
		// Name the declared budget instead of Argo's generic child failure.
		if storage != "" && !model.IsExecutionDeadlineReason(completion.Reason) {
			completion.Reason = storage
		}
		if reason != "" {
			completion.FailedStage, completion.FailedStep = failedStage, failedStep
		}
		return
	}
	if reason != "" {
		completion.Infrastructure = true
		completion.Reason = reason
	}
}

// podStartupDeadlineReason classifies a step-level Pod deadline kill that the
// step command did not cause (#1499). Kubernetes counts activeDeadlineSeconds
// from Pod start, so scheduling contention, image pulls, init containers and
// the post-exit wait sidecar are charged to the Pod. Three kills are
// infrastructure: main exited 0 (the step itself succeeded); main never
// started (still being created, or rewritten by kubelet's TerminatePod to
// ContainerStatusUnknown/137 without a container or start time); or the Pod
// deadline itself stopped main before main had used the declared step budget
// Build recorded beside the floored Pod deadline. A main container that exited
// nonzero on its own before the deadline, that used its whole declared budget,
// or that never started because of an image or configuration error remains
// the step's own failure, and a workflow-deadline kill is never reclassified.
func podStartupDeadlineReason(workflow *wfv1.Workflow, node wfv1.NodeStatus, pod *corev1.Pod, stage, step string) string {
	if !stepPodBound(workflow, node, pod) || pod.Status.Phase != corev1.PodFailed || pod.Status.Reason != "DeadlineExceeded" || pod.Spec.ActiveDeadlineSeconds == nil || workflowDeadlineExceeded(workflow) {
		return ""
	}
	var main *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[i]
		if status.Name != mainContainerName {
			continue
		}
		if main != nil {
			return ""
		}
		main = status
	}
	if main == nil {
		return ""
	}
	limit := *pod.Spec.ActiveDeadlineSeconds
	if main.State.Terminated != nil && main.State.Terminated.ExitCode == 0 && main.State.Running == nil && main.State.Waiting == nil {
		return model.StartupDeadlineReasonPrefix + fmt.Sprintf("step %s/%s exited 0, but its Pod deadline of %ds expired outside the step command (Pod start-up or result upload).", stage, step, limit)
	}
	declared, ok := declaredStepDeadline(pod)
	if !ok {
		return ""
	}
	var runtime time.Duration
	switch {
	case mainNeverStarted(main):
		// The whole Pod deadline went to start-up.
	case main.State.Terminated != nil && main.State.Running == nil && main.State.Waiting == nil:
		t := main.State.Terminated
		if t.StartedAt.IsZero() || t.FinishedAt.IsZero() || t.FinishedAt.Before(&t.StartedAt) || !killedByPodDeadline(pod, t) {
			return ""
		}
		runtime = t.FinishedAt.Sub(t.StartedAt.Time)
	default:
		return ""
	}
	if runtime >= time.Duration(declared)*time.Second {
		return ""
	}
	return model.StartupDeadlineReasonPrefix + fmt.Sprintf("step %s/%s ran %s of its %ds budget; its Pod deadline of %ds expired during Pod start-up (scheduling, image pull, init containers).", stage, step, runtime.Truncate(time.Second), declared, limit)
}

// podDeadlineClockSkew tolerates the kubelet recording main's finish a moment
// before its own Pod deadline check, measured from the Pod's start time.
const podDeadlineClockSkew = 2 * time.Second

// mainNeverStarted accepts only the shapes of a container that was still being
// started when the Pod deadline expired. Kubelet's TerminatePod rewrites every
// non-terminated container of an initialized Pod to ContainerStatusUnknown with
// exit 137 and no start time; a container still being created keeps its
// ContainerCreating or PodInitializing waiting reason. Image and configuration
// errors (ErrImagePull, InvalidImageName, CreateContainerConfigError, ...) are
// the repository's own failure and never count as start-up.
func mainNeverStarted(main *corev1.ContainerStatus) bool {
	if main.ContainerID != "" || main.LastTerminationState.Terminated != nil || main.State.Running != nil {
		return false
	}
	if waiting := main.State.Waiting; waiting != nil {
		return main.State.Terminated == nil && (waiting.Reason == "ContainerCreating" || waiting.Reason == "PodInitializing")
	}
	t := main.State.Terminated
	return t != nil && t.Reason == "ContainerStatusUnknown" && t.ExitCode == 137 && t.StartedAt.IsZero() && t.ContainerID == ""
}

// killedByPodDeadline reports whether the Pod deadline stopped main, rather
// than main exiting on its own. Main must have finished at or after the Pod
// deadline; without a Pod start time only a plain SIGKILL exit (137, not an
// out-of-memory kill) is accepted. A nonzero exit before the deadline is
// always the step's own failure.
func killedByPodDeadline(pod *corev1.Pod, t *corev1.ContainerStateTerminated) bool {
	if t.Reason == "OOMKilled" {
		return false
	}
	if pod.Status.StartTime == nil || pod.Status.StartTime.IsZero() {
		return t.ExitCode == 137
	}
	deadline := pod.Status.StartTime.Add(time.Duration(*pod.Spec.ActiveDeadlineSeconds) * time.Second)
	return !t.FinishedAt.Time.Before(deadline.Add(-podDeadlineClockSkew))
}
