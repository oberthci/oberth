package argojob

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/oberthci/oberth/internal/model"
)

// collectStepDiagnostics runs before completion is handed to the scheduler and
// before its cleanup. Source-owned Workflows retain Pods until Workflow TTL;
// external GC/read failures are explicit unavailable observations. Diagnostics
// keep their typed projection separate from infrastructure classification.
// One total five-second budget bounds all API reads, including large workflows
// and recovery.
func (controller *Controller) collectStepDiagnostics(ctx context.Context, workflow *wfv1.Workflow, completion *Completion) {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	pods, snapshotReason := controller.timingPodSnapshot(readCtx, workflow)
	for i := range completion.Steps {
		step := &completion.Steps[i]
		node, ok := workflow.Status.Nodes[step.NodeID]
		var pod *corev1.Pod
		reason := ""
		switch {
		case step.AttemptAmbiguous:
			reason = "ambiguous-attempt"
		case !ok || node.ID != step.NodeID:
			reason = "binding-mismatch"
		default:
			for id, candidate := range workflow.Status.Nodes {
				if candidate.ID == node.ID && id != node.ID {
					reason = "ambiguous-attempt"
					break
				}
			}
			if reason == "" {
				switch {
				case node.Type != wfv1.NodeTypePod || step.PodName == "":
					reason = "not-executed"
				case snapshotReason != "":
					reason = snapshotReason
				default:
					pod = pods[step.PodName]
					if pod == nil {
						reason = "observation-unavailable"
					}
				}
			}
		}
		at := time.Now().UTC()
		if reason != "" {
			step.Termination = &model.StepDiagnostic{Unavailable: reason, ObservedAt: &at}
		} else {
			step.Termination = projectStepDiagnostic(workflow, node, pod, at)
			if step.Termination.Available && step.Termination.Container.ContainerKind == "init" {
				step.ExitCode = int(step.Termination.Container.ExitCode)
			}
			step.Usage = projectStepUsage(workflow, node, pod)
			if reason := podInfrastructureReason(workflow, node, pod); reason != "" {
				step.InfrastructureReason = reason
			}
		}
		step.Timing = projectStepTiming(workflow, node, pod, *step, step.Termination, at)
	}
	// A timed-out API read must still durably retain its explicit unknowns.
	// Persistence gets its own bounded write context, not the expired read one.
	writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer writeCancel()
	controller.retainStepTimings(writeCtx, completion)
	completion.classifyInfrastructure()
}

// Every page belongs to one consistent Kubernetes LIST snapshot. No partial
// snapshot is authoritative: API errors, repeated cursors, duplicates and bounds
// leave every executed step explicitly unavailable. Per-Pod identity checks
// remain independent of the label selector, which only limits the read scope.
func (controller *Controller) timingPodSnapshot(ctx context.Context, workflow *wfv1.Workflow) (map[string]*corev1.Pod, string) {
	if controller.kube == nil {
		return nil, "observation-unavailable"
	}
	const pageSize = 128
	const maxPages = 16
	pods := map[string]*corev1.Pod{}
	cursors := map[string]bool{}
	cursor, version := "", ""
	for page := 0; page < maxPages; page++ {
		list, err := controller.kube.CoreV1().Pods(workflow.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labels.Set{"workflows.argoproj.io/workflow": workflow.Name}.AsSelector().String(),
			Limit:         pageSize, Continue: cursor,
		})
		if err != nil {
			if page == 0 {
				return nil, "observation-unavailable"
			}
			return nil, "snapshot-incomplete"
		}
		if page > 0 && version != list.ResourceVersion {
			return nil, "snapshot-incomplete"
		}
		version = list.ResourceVersion
		if len(list.Items) > model.MaxTimingTasks-len(pods) {
			return nil, "snapshot-incomplete"
		}
		for i := range list.Items {
			pod := &list.Items[i]
			if _, duplicate := pods[pod.Name]; duplicate {
				return nil, "snapshot-incomplete"
			}
			pods[pod.Name] = pod
		}
		if list.Continue == "" {
			return pods, ""
		}
		if version == "" || cursors[list.Continue] {
			return nil, "snapshot-incomplete"
		}
		cursors[list.Continue] = true
		cursor = list.Continue
	}
	return nil, "snapshot-incomplete"
}

func projectStepDiagnostic(workflow *wfv1.Workflow, node wfv1.NodeStatus, pod *corev1.Pod, observed time.Time) *model.StepDiagnostic {
	unavailable := func(reason string) *model.StepDiagnostic {
		return &model.StepDiagnostic{Unavailable: reason, ObservedAt: &observed}
	}
	if !stepPodIdentityBound(workflow, node, pod) {
		return unavailable("binding-mismatch")
	}
	runID, _, _ := WorkflowMeta(workflow)
	var main *corev1.Container
	kind := ""
	status := failedInitContainer(pod)
	if status != nil {
		kind = "init"
		for i := range pod.Spec.InitContainers {
			if pod.Spec.InitContainers[i].Name == status.Name {
				if main != nil {
					return unavailable("unsupported-shape")
				}
				main = &pod.Spec.InitContainers[i]
			}
		}
		if main == nil {
			return unavailable("unsupported-shape")
		}
	} else {
		seen := map[string]bool{}
		for i := range pod.Spec.Containers {
			c := &pod.Spec.Containers[i]
			if seen[c.Name] || (c.Name != "main" && c.Name != "wait") {
				return unavailable("unsupported-shape")
			}
			seen[c.Name] = true
			if c.Name == "main" {
				main = c
			}
		}
		if main == nil {
			return unavailable("unsupported-shape")
		}
		seen = map[string]bool{}
		for i := range pod.Status.ContainerStatuses {
			c := &pod.Status.ContainerStatuses[i]
			if seen[c.Name] || (c.Name != "main" && c.Name != "wait") {
				return unavailable("unsupported-shape")
			}
			seen[c.Name] = true
			if c.Name == "main" {
				status = c
			}
		}
	}
	if status == nil || status.State.Terminated == nil || status.State.Running != nil || status.State.Waiting != nil {
		return unavailable("termination-unavailable")
	}
	t := status.State.Terminated
	if status.ContainerID == "" || status.ContainerID != t.ContainerID || status.Ready || t.StartedAt.IsZero() || t.FinishedAt.IsZero() {
		return unavailable("invalid-termination")
	}
	if kind != "init" && node.Outputs != nil && node.Outputs.ExitCode != nil {
		code, err := parseExitCode(*node.Outputs.ExitCode)
		if err != nil || code != int(t.ExitCode) {
			return unavailable("invalid-termination")
		}
	}
	reason := "other"
	switch t.Reason {
	case "Completed", "Error", "OOMKilled", "ContainerCannotRun":
		reason = t.Reason
	}
	podReason := "other"
	switch pod.Status.Reason {
	case "":
		podReason = "none"
	case "DeadlineExceeded":
		podReason = "DeadlineExceeded"
	}
	// Refuse quantities outside a bounded positive representable range before
	// Value/MilliValue; their scaled integer conversion must not overflow.
	for _, list := range []corev1.ResourceList{main.Resources.Requests, main.Resources.Limits} {
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			if q, exists := list[name]; exists {
				limit := resource.NewQuantity(1<<50, resource.DecimalSI)
				if name == corev1.ResourceCPU {
					limit = resource.NewMilliQuantity(1<<50, resource.DecimalSI)
				}
				if q.Sign() < 0 || q.Cmp(*limit) > 0 {
					return unavailable("invalid-termination")
				}
			}
		}
	}
	evidence := &model.StepTermination{RunID: runID, Namespace: workflow.Namespace, WorkflowName: workflow.Name, WorkflowUID: string(workflow.UID), NodeID: node.ID, PodName: pod.Name, PodUID: string(pod.UID), ContainerName: status.Name, ContainerKind: kind, ContainerID: status.ContainerID, RestartCount: status.RestartCount, Reason: reason, PodReason: podReason, ExitCode: t.ExitCode, Signal: t.Signal, StartedAt: t.StartedAt.Time, FinishedAt: t.FinishedAt.Time, ActiveDeadlineSeconds: pod.Spec.ActiveDeadlineSeconds}
	if value, ok := main.Resources.Requests[corev1.ResourceCPU]; ok {
		n := value.MilliValue()
		evidence.CPURequestMilli = &n
	}
	if value, ok := main.Resources.Limits[corev1.ResourceCPU]; ok {
		n := value.MilliValue()
		evidence.CPULimitMilli = &n
	}
	if value, ok := main.Resources.Requests[corev1.ResourceMemory]; ok {
		n := value.Value()
		evidence.MemoryRequestBytes = &n
	}
	if value, ok := main.Resources.Limits[corev1.ResourceMemory]; ok {
		n := value.Value()
		evidence.MemoryLimitBytes = &n
	}
	evidence.Message = strings.ToValidUTF8(t.Message, "�")
	for len(evidence.Message) > 1536 {
		_, n := utf8.DecodeLastRuneInString(evidence.Message)
		evidence.Message = evidence.Message[:len(evidence.Message)-n]
		evidence.MessageTruncated = true
	}
	result := &model.StepDiagnostic{Available: true, ObservedAt: &observed, Container: evidence}
	// Keep the entire bounded diagnostic valid even for escaped control bytes.
	for result.Validate() != nil && len(evidence.Message) > 0 {
		_, n := utf8.DecodeLastRuneInString(evidence.Message)
		evidence.Message = evidence.Message[:len(evidence.Message)-n]
		evidence.MessageTruncated = true
	}
	if result.Validate() != nil {
		return unavailable("invalid-termination")
	}
	return result
}

func stepPodBound(workflow *wfv1.Workflow, node wfv1.NodeStatus, pod *corev1.Pod) bool {
	return node.Phase.Completed() && stepPodIdentityBound(workflow, node, pod)
}
func stepPodIdentityBound(workflow *wfv1.Workflow, node wfv1.NodeStatus, pod *corev1.Pod) bool {
	_, _, ok := WorkflowMeta(workflow)
	if !ok || workflow.UID == "" || pod == nil || pod.UID == "" || pod.Namespace != workflow.Namespace || pod.Name != PodName(workflow, node) || node.Type != wfv1.NodeTypePod || pod.Annotations["workflows.argoproj.io/node-id"] != node.ID || pod.Annotations["workflows.argoproj.io/node-name"] != node.Name {
		return false
	}
	owners := 0
	for _, owner := range pod.OwnerReferences {
		if owner.Controller != nil && *owner.Controller {
			owners++
			if owner.APIVersion != "argoproj.io/v1alpha1" || owner.Kind != "Workflow" || owner.Name != workflow.Name || owner.UID != workflow.UID {
				return false
			}
		}
	}
	return owners == 1
}

// Main never started when a setup init fails. Only one current failed init
// status is an unambiguous source; never substitute last-state or wait logs.
func failedInitContainer(pod *corev1.Pod) *corev1.ContainerStatus {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == mainContainerName && (s.ContainerID != "" || s.State.Running != nil || s.State.Terminated != nil) {
			return nil
		}
	}
	var failed *corev1.ContainerStatus
	for i := range pod.Status.InitContainerStatuses {
		s := &pod.Status.InitContainerStatuses[i]
		if s.State.Terminated != nil && s.State.Terminated.ExitCode != 0 && s.State.Running == nil && s.State.Waiting == nil {
			if failed != nil {
				return nil
			}
			failed = s
		}
	}
	return failed
}
