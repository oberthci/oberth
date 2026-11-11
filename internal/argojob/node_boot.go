package argojob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
)

type NodeBootRecorder interface {
	ObserveNodeBoot(context.Context, model.NodeBootObservation, bool) (*model.NodeReboot, error)
}

func (controller *Controller) WithNodeBootRecorder(recorder NodeBootRecorder) *Controller {
	controller.nodeBoots = recorder
	return controller
}

// Observe only owned step Pods, never infer a reboot from readiness changes or
// an exit code. One bounded read snapshot supplies all attempts on each node.
func (controller *Controller) nodeRebootReasons(ctx context.Context, workflow *wfv1.Workflow, pods map[string]*corev1.Pod, resumedAt time.Time) map[string]string {
	reasons := map[string]string{}
	if controller.nodeBoots == nil || controller.kube == nil {
		return reasons
	}
	nodes := map[string]*corev1.Node{}
	runID, _, _ := WorkflowMeta(workflow)
	for _, node := range workflow.Status.Nodes {
		if node.Type != wfv1.NodeTypePod {
			continue
		}
		pod := pods[PodName(workflow, node)]
		if !stepPodIdentityBound(workflow, node, pod) || pod.Spec.NodeName == "" {
			continue
		}
		var main *corev1.ContainerStatus
		for i := range pod.Status.ContainerStatuses {
			if pod.Status.ContainerStatuses[i].Name == mainContainerName {
				if main != nil {
					main = nil
					break
				}
				main = &pod.Status.ContainerStatuses[i]
			}
		}
		if main == nil || main.State.Waiting != nil {
			continue
		}
		var started time.Time
		running := main.State.Running != nil && main.State.Terminated == nil
		if running {
			started = main.State.Running.StartedAt.Time
		} else if main.State.Terminated != nil && main.State.Running == nil {
			started = main.State.Terminated.StartedAt.Time
		} else {
			continue
		}
		if running && started.IsZero() {
			continue
		}
		n, seen := nodes[pod.Spec.NodeName]
		if !seen {
			observed, err := controller.kube.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
			if err == nil {
				n = observed
			}
			nodes[pod.Spec.NodeName] = n
		}
		if n == nil || n.Name != pod.Spec.NodeName || n.DeletionTimestamp != nil {
			continue
		}
		// metav1.Time is serialized to whole seconds by the Kubernetes API.
		// Canonicalize the binding even when a client supplies finer precision.
		o := model.NodeBootObservation{RunID: runID, WorkflowName: workflow.Name, WorkflowUID: string(workflow.UID), NodeID: node.ID, PodUID: string(pod.UID), NodeName: n.Name, NodeUID: string(n.UID), BootID: n.Status.NodeInfo.BootID, StartedAt: started.Truncate(time.Second), ObservedAt: time.Now().UTC()}
		if !o.Valid() {
			continue
		}
		reboot, err := controller.nodeBoots.ObserveNodeBoot(ctx, o, running)
		if err != nil || reboot == nil || running {
			continue
		}
		d := projectStepDiagnostic(workflow, node, pod, o.ObservedAt)
		// Known command failures and OOMs keep their code/resource diagnosis,
		// even when the same node subsequently reboots. An ambiguous kubelet
		// termination qualifies only with the earlier running-boot observation.
		t := main.State.Terminated
		if t.ExitCode <= 0 || t.FinishedAt.IsZero() || t.FinishedAt.After(o.ObservedAt) || !t.FinishedAt.Time.Truncate(time.Second).Add(time.Second).After(reboot.After) || (node.Phase != wfv1.NodeFailed && node.Phase != wfv1.NodeError) {
			continue
		}
		ambiguous := d.Available && d.Container.ContainerKind != "init" && d.Container.Reason == "other"
		// After losing its runtime state, kubelet can synthesize an unknown
		// termination without a container ID/start time. The retained running
		// baseline and an owned Never-restarting Pod still bind this attempt.
		missingRuntime := main.ContainerID == "" || t.ContainerID == "" || t.StartedAt.IsZero()
		consistentExit := true
		if node.Outputs != nil && node.Outputs.ExitCode != nil {
			code, err := parseExitCode(*node.Outputs.ExitCode)
			consistentExit = err == nil && code == int(t.ExitCode)
		}
		if !d.Available && d.Unavailable == "invalid-termination" && missingRuntime && consistentExit && t.Signal >= 0 && t.Signal <= 64 && pod.Spec.RestartPolicy == corev1.RestartPolicyNever && main.RestartCount == 0 && !main.Ready && (t.Reason == "" || t.Reason == "other" || t.Reason == "Unknown" || t.Reason == "ContainerStatusUnknown") && (main.ContainerID == "" || t.ContainerID == "" || main.ContainerID == t.ContainerID) {
			ambiguous = true
		}
		if !ambiguous && d.Available && d.Container.ContainerKind != "init" && d.Container.Reason == "Error" && d.Container.ExitCode == 255 && !resumedAt.IsZero() && !started.After(resumedAt) {
			ambiguous = controller.stepOutputEmpty(ctx, workflow, node, pod, resumedAt)
		}
		if !ambiguous {
			continue
		}
		reasons[node.ID] = model.NodeRebootReasonPrefix + fmt.Sprintf("node %s restarted after %s; new boot first observed at %s", reboot.NodeName, reboot.After.Format(time.RFC3339Nano), reboot.ObservedAt.Format(time.RFC3339Nano))
	}
	return reasons
}

func (controller *Controller) observeRunningNodeBoots(ctx context.Context, workflow *wfv1.Workflow) {
	if controller.nodeBoots == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	pods, reason := controller.timingPodSnapshot(ctx, workflow)
	if reason == "" {
		controller.nodeRebootReasons(ctx, workflow, pods, time.Time{})
	}
}

func (controller *Controller) stepOutputEmpty(ctx context.Context, workflow *wfv1.Workflow, node wfv1.NodeStatus, pod *corev1.Pod, resumedAt time.Time) bool {
	if controller.logs == nil && controller.followLogs == nil {
		return false
	}
	samePod := func() bool {
		current, err := controller.kube.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		return err == nil && stepPodIdentityBound(workflow, node, current) && current.UID == pod.UID
	}
	if !samePod() {
		return false
	}
	var reader io.ReadCloser
	var err error
	if controller.followLogs != nil {
		since := metav1.NewTime(resumedAt)
		reader, err = controller.followLogs(ctx, pod.Namespace, pod.Name, corev1.PodLogOptions{Container: mainContainerName, SinceTime: &since})
	} else {
		reader, err = controller.logs(ctx, pod.Namespace, pod.Name, mainContainerName)
	}
	if err != nil {
		return false
	}
	defer func() { _ = reader.Close() }()
	var one [1]byte
	n, err := io.ReadFull(reader, one[:])
	return n == 0 && errors.Is(err, io.EOF) && samePod()
}
