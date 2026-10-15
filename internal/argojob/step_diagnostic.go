package argojob

import (
	"context"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
)

// collectStepDiagnostics runs before completion is handed to the scheduler and
// before its cleanup. Source-owned Workflows retain Pods until Workflow TTL;
// external GC/read failures are explicit unavailable observations. Diagnostics
// never replace or modify the pipeline verdict. One total five-second budget
// bounds all API reads, including large workflows and recovery.
func (controller *Controller) collectStepDiagnostics(ctx context.Context, workflow *wfv1.Workflow, completion *Completion) {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for i := range completion.Steps {
		step := &completion.Steps[i]
		unavailable := func(reason string) {
			at := time.Now().UTC()
			step.Termination = &model.StepDiagnostic{Unavailable: reason, ObservedAt: &at}
		}
		if step.AttemptAmbiguous {
			unavailable("ambiguous-attempt")
			continue
		}
		node, ok := workflow.Status.Nodes[step.NodeID]
		if !ok || node.ID != step.NodeID {
			unavailable("binding-mismatch")
			continue
		}
		duplicateID := false
		for id, candidate := range workflow.Status.Nodes {
			if candidate.ID == node.ID && id != node.ID {
				duplicateID = true
			}
		}
		if duplicateID {
			unavailable("ambiguous-attempt")
			continue
		}
		if node.Type != wfv1.NodeTypePod || step.PodName == "" {
			unavailable("not-executed")
			continue
		}
		if controller.kube == nil || readCtx.Err() != nil {
			unavailable("observation-unavailable")
			continue
		}
		pod, err := controller.kube.CoreV1().Pods(workflow.Namespace).Get(readCtx, step.PodName, metav1.GetOptions{})
		if err != nil {
			unavailable("observation-unavailable")
			continue
		}
		step.Termination = projectStepDiagnostic(workflow, node, pod, time.Now().UTC())
	}
}

func projectStepDiagnostic(workflow *wfv1.Workflow, node wfv1.NodeStatus, pod *corev1.Pod, observed time.Time) *model.StepDiagnostic {
	unavailable := func(reason string) *model.StepDiagnostic {
		return &model.StepDiagnostic{Unavailable: reason, ObservedAt: &observed}
	}
	runID, _, ok := WorkflowMeta(workflow)
	if !ok || workflow.UID == "" || pod == nil || pod.UID == "" || pod.Namespace != workflow.Namespace || pod.Name != PodName(workflow, node) || node.Type != wfv1.NodeTypePod || !node.Phase.Completed() || pod.Annotations["workflows.argoproj.io/node-id"] != node.ID || pod.Annotations["workflows.argoproj.io/node-name"] != node.Name {
		return unavailable("binding-mismatch")
	}
	owners := 0
	for _, owner := range pod.OwnerReferences {
		if owner.Controller != nil && *owner.Controller {
			owners++
			if owner.APIVersion != "argoproj.io/v1alpha1" || owner.Kind != "Workflow" || owner.Name != workflow.Name || owner.UID != workflow.UID {
				return unavailable("binding-mismatch")
			}
		}
	}
	if owners != 1 {
		return unavailable("binding-mismatch")
	}
	var main *corev1.Container
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
	var status *corev1.ContainerStatus
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
	if status == nil || status.State.Terminated == nil || status.State.Running != nil || status.State.Waiting != nil {
		return unavailable("termination-unavailable")
	}
	t := status.State.Terminated
	if status.ContainerID == "" || status.ContainerID != t.ContainerID || status.Ready || t.StartedAt.IsZero() || t.FinishedAt.IsZero() {
		return unavailable("invalid-termination")
	}
	if node.Outputs != nil && node.Outputs.ExitCode != nil {
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
	evidence := &model.StepTermination{RunID: runID, Namespace: workflow.Namespace, WorkflowName: workflow.Name, WorkflowUID: string(workflow.UID), NodeID: node.ID, PodName: pod.Name, PodUID: string(pod.UID), ContainerName: "main", ContainerID: status.ContainerID, RestartCount: status.RestartCount, Reason: reason, PodReason: podReason, ExitCode: t.ExitCode, Signal: t.Signal, StartedAt: t.StartedAt.Time, FinishedAt: t.FinishedAt.Time, ActiveDeadlineSeconds: pod.Spec.ActiveDeadlineSeconds}
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
	result := &model.StepDiagnostic{Available: true, ObservedAt: &observed, Container: evidence}
	if result.Validate() != nil {
		return unavailable("invalid-termination")
	}
	return result
}
