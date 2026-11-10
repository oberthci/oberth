package argojob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kexec "k8s.io/client-go/util/exec"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// Hold operations the run_hold_* tools reach through app.ArgoJobs. They act
// only on Pods that carry the server-owned hold annotation and the emissary
// pause, in the pipeline namespace, through the same pods/exec and pods
// delete verbs the seeder and executor-identity retry already use.

// argoWorkflowLabel is Argo's own Pod label naming the owning Workflow.
const argoWorkflowLabel = "workflows.argoproj.io/workflow"

// argoNodeNameAnnotation is Argo's Pod annotation with the node name; a
// retry child ends in "(N)".
const argoNodeNameAnnotation = "workflows.argoproj.io/node-name"

var retryAttemptSuffix = regexp.MustCompile(`\((\d+)\)$`)

// ErrNotHeld reports a Pod that is not a parked assist attempt.
var ErrNotHeld = errors.New("argojob: pod is not a held assist attempt")

// AssistPolicy reports the effective administrator policy.
func (controller *Controller) AssistPolicy() model.AssistPolicy {
	assist := controller.config.Assist.Effective()
	return model.AssistPolicy{Enabled: controller.config.Assist.Enabled, HoldTTL: assist.HoldTTL, MaxConcurrentHolds: assist.MaxConcurrentHolds,
		ExecTimeout: assist.ExecTimeout, ExecOutputBytes: assist.ExecOutputBytes, PatchMaxBytes: assist.PatchMaxBytes, PatchMaxFiles: assist.PatchMaxFiles,
		ScratchBytes: max(assist.ScratchBytes, 0)}
}

// ListAssistHolds returns the parked attempts of one Workflow, oldest first.
func (controller *Controller) ListAssistHolds(ctx context.Context, workflowName string) ([]model.AssistHold, error) {
	if controller.kube == nil {
		return nil, errors.New("argojob: hold inspection needs the Kubernetes client")
	}
	if strings.TrimSpace(workflowName) == "" {
		return nil, errors.New("argojob: workflow name is required")
	}
	pods, err := controller.kube.CoreV1().Pods(controller.Namespace()).List(ctx, metav1.ListOptions{LabelSelector: argoWorkflowLabel + "=" + workflowName})
	if err != nil {
		return nil, fmt.Errorf("list workflow pods: %w", err)
	}
	var holds []model.AssistHold
	for index := range pods.Items {
		if hold, ok := assistHoldOf(&pods.Items[index]); ok {
			holds = append(holds, hold)
		}
	}
	sort.Slice(holds, func(i, j int) bool { return holds[i].HeldSince.Before(holds[j].HeldSince) })
	return holds, nil
}

// assistHoldOf describes a Pod as a hold when it is one and its main
// container is running (parked or already released).
func assistHoldOf(pod *corev1.Pod) (model.AssistHold, bool) {
	if !AssistHoldPod(pod) || pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
		return model.AssistHold{}, false
	}
	var main *corev1.Container
	for index := range pod.Spec.Containers {
		if pod.Spec.Containers[index].Name == mainContainerName {
			main = &pod.Spec.Containers[index]
		}
	}
	if main == nil {
		return model.AssistHold{}, false
	}
	hold := model.AssistHold{Pod: pod.Name, Node: pod.Annotations[argoNodeNameAnnotation], SourcePath: SourceMountPath, PatchCapable: hasUsageBinary(main)}
	for _, variable := range main.Env {
		if variable.Name == "OBERTH_STEP" {
			hold.Step = variable.Value
		}
	}
	if hold.Step == "" {
		hold.Step = nodeStepName(pod.Labels[argoWorkflowLabel], hold.Node)
	}
	// Only a retry child (node "...(N)", N >= 1) can be a hold: the held
	// patch branch is never applied to attempt 1, and the pause variable is
	// refused from repositories, so an attempt-1 Pod carrying it is not ours.
	match := retryAttemptSuffix.FindStringSubmatch(hold.Node)
	if match == nil {
		return model.AssistHold{}, false
	}
	hold.Attempt, _ = strconv.Atoi(match[1])
	if hold.Attempt < 1 {
		return model.AssistHold{}, false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == mainContainerName && status.State.Running != nil {
			hold.HeldSince = status.State.Running.StartedAt.Time
		}
	}
	if hold.HeldSince.IsZero() {
		return model.AssistHold{}, false
	}
	if pod.Spec.ActiveDeadlineSeconds != nil {
		start := hold.HeldSince
		if pod.Status.StartTime != nil {
			start = pod.Status.StartTime.Time
		}
		hold.Deadline = start.Add(time.Duration(*pod.Spec.ActiveDeadlineSeconds) * time.Second)
	}
	return hold, true
}

// nodeStepName is the step a Pod's Argo node name stands for when the Pod
// carries no OBERTH_STEP: the same rule the failure display applies to the
// node tree (stageAndStep), i.e. the node's own display name, the last
// dotted segment of "<workflow>.<boundary>.<task>(N)", without the retry
// suffix. A hold recorded under it matches the run's FailedStep.
func nodeStepName(workflow, node string) string {
	name := strings.TrimPrefix(node, workflow+".")
	if index := strings.LastIndex(name, "."); index >= 0 {
		name = name[index+1:]
	}
	return argoworkflow.StepName(stripRetrySuffix(name))
}

// heldPod loads and verifies a hold target.
func (controller *Controller) heldPod(ctx context.Context, name string) (*corev1.Pod, model.AssistHold, error) {
	if controller.kube == nil {
		return nil, model.AssistHold{}, errors.New("argojob: hold operations need the Kubernetes client")
	}
	pod, err := controller.kube.CoreV1().Pods(controller.Namespace()).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, model.AssistHold{}, fmt.Errorf("read held pod: %w", err)
	}
	hold, ok := assistHoldOf(pod)
	if !ok {
		return nil, model.AssistHold{}, ErrNotHeld
	}
	return pod, hold, nil
}

// ExecAssistHold runs argv in the held main container with a deadline and
// bounded captured output. The exit code comes from the exec stream.
func (controller *Controller) ExecAssistHold(ctx context.Context, request model.AssistExecRequest) (model.AssistExecResult, error) {
	if len(request.Argv) == 0 {
		return model.AssistExecResult{}, errors.New("argojob: exec needs an argv")
	}
	if controller.seeder == nil || controller.seeder.exec == nil {
		return model.AssistExecResult{}, errors.New("argojob: hold exec needs the pods/exec streamer")
	}
	if _, _, err := controller.heldPod(ctx, request.Pod); err != nil {
		return model.AssistExecResult{}, err
	}
	timeout := request.Timeout
	if timeout <= 0 {
		timeout = controller.config.Assist.Effective().ExecTimeout
	}
	limit := request.OutputLimit
	if limit <= 0 {
		limit = controller.config.Assist.Effective().ExecOutputBytes
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr := &boundedBuffer{limit: limit}, &boundedBuffer{limit: limit}
	var stdin io.Reader
	if len(request.Stdin) > 0 {
		stdin = bytes.NewReader(request.Stdin)
	}
	started := time.Now()
	err := controller.seeder.exec(execCtx, controller.Namespace(), request.Pod, mainContainerName, request.Argv, stdin, stdout, stderr)
	result := model.AssistExecResult{Stdout: stdout.String(), Stderr: stderr.String(), Truncated: stdout.truncated || stderr.truncated,
		StdoutBytes: stdout.total, StderrBytes: stderr.total, Duration: time.Since(started)}
	var exit kexec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		result.ExitCode = exit.ExitStatus()
	case execCtx.Err() != nil:
		result.TimedOut = true
		result.ExitCode = -1
	default:
		return result, fmt.Errorf("exec in held pod: %w", err)
	}
	return result, nil
}

// ReleaseAssistHold creates the emissary's release marker so the parked
// executor runs the step command. The server's helper does it where it is
// mounted; otherwise the image's own touch is asked, argv-only.
func (controller *Controller) ReleaseAssistHold(ctx context.Context, pod string) error {
	_, hold, err := controller.heldPod(ctx, pod)
	if err != nil {
		return err
	}
	argv := []string{"touch", AssistReleasePath}
	if hold.PatchCapable {
		argv = []string{usageBinPath, "internal-hold-fs", "release", "--path", AssistReleasePath}
	}
	result, err := controller.ExecAssistHold(ctx, model.AssistExecRequest{Pod: pod, Argv: argv, Timeout: 30 * time.Second, OutputLimit: 4096})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("argojob: release marker failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// AbandonAssistHold deletes the held Pod so the node fails normally.
func (controller *Controller) AbandonAssistHold(ctx context.Context, pod string) error {
	if _, _, err := controller.heldPod(ctx, pod); err != nil {
		return err
	}
	return controller.kube.CoreV1().Pods(controller.Namespace()).Delete(ctx, pod, metav1.DeleteOptions{})
}

// assistSweepSelector selects every branch-CI step Pod: Build stamps
// triggerLabel with request.Trigger, and holds exist only on TriggerCI runs.
var assistSweepSelector = triggerLabel + "=" + string(pipeline.TriggerCI)

// EnforceAssistHolds bounds parked attempts and returns what it abandoned.
// Holds already released by their agent are running commands and are
// neither aged nor counted.
//
//   - TTL: an unreleased idle hold whose HeldSince is assist.holdTTL old is
//     abandoned. A hold with an agent exec, patch or continue ages from its
//     last action instead, but is abandoned at AssistHoldMaxAgeFactor TTLs
//     after HeldSince whatever its activity. The Pod deadline is the step
//     budget plus that cap, so the server, not the kubelet, enforces both.
//   - Cap: above assist.maxConcurrentHolds the newest idle holds are
//     abandoned first. The sweep runs every few seconds, so in practice a
//     hold that parks over the cap fails at once, as if the run were
//     unassisted, and the holds agents already have keep their slots. A hold
//     with an exec, patch or continue in the last model.AssistActivityWindow
//     is never abandoned by the cap; it still counts toward it.
//
// activity is a snapshot read before the Pod list. When recheck is set, each
// chosen victim's activity is read again right before its delete and the
// victim is spared if that fresh record no longer condemns it (released,
// for the TTL a newer action that extends it, for the cap worked inside the
// window), closing the window in which an
// action landing between snapshot and delete would be killed.
func (controller *Controller) EnforceAssistHolds(ctx context.Context, activity map[string]model.AssistPodActivity, recheck model.AssistActivityRecheck, now time.Time) ([]model.AssistAbandon, error) {
	if controller.kube == nil || !controller.config.Assist.Enabled {
		return nil, nil
	}
	assist := controller.config.Assist.Effective()
	pods, err := controller.kube.CoreV1().Pods(controller.Namespace()).List(ctx, metav1.ListOptions{LabelSelector: assistSweepSelector})
	if err != nil {
		return nil, fmt.Errorf("list branch pods: %w", err)
	}
	var abandoned []model.AssistAbandon
	released := map[string]bool{} // released between snapshot and recheck
	abandon := func(sweep assistSweepHold, reason string) (bool, error) {
		if recheck != nil {
			fresh, err := recheck(ctx, sweep.runID, sweep.hold.Pod)
			released[sweep.hold.Pod] = err == nil && fresh.Released
			if err != nil || !assistVictimDue(reason, fresh, assist.HoldTTL, sweep.hold, now) {
				return false, nil
			}
		}
		if err := controller.kube.CoreV1().Pods(controller.Namespace()).Delete(ctx, sweep.hold.Pod, metav1.DeleteOptions{}); err != nil {
			return false, fmt.Errorf("abandon hold %s: %w", sweep.hold.Pod, err)
		}
		abandoned = append(abandoned, model.AssistAbandon{RunID: sweep.runID, Pod: sweep.hold.Pod, Step: sweep.hold.Step, Reason: reason,
			HeldSince: sweep.hold.HeldSince, AbandonedAt: now, FailedAttempt: sweep.hold.Attempt - 1, FailedExitCode: sweep.failedExit})
		return true, nil
	}
	var holds []assistSweepHold
	seen := map[string]bool{}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if evicted, ok := assistEvictedOf(pod); ok {
			if eviction, record := controller.assistEviction(ctx, pods.Items, pod, evicted, activity, recheck, now); record {
				abandoned = append(abandoned, eviction)
			}
			continue
		}
		hold, ok := assistHoldOf(pod)
		if !ok || activity[hold.Pod].Released {
			continue
		}
		sweep := assistSweepHold{hold: hold, runID: pod.Labels[runLabel], failedExit: failedAttemptExit(pods.Items, pod, hold)}
		seen[hold.Pod] = true
		controller.noteAssistHold(sweep)
		if assistVictimDue(model.AssistAbandonTTL, activity[hold.Pod], assist.HoldTTL, hold, now) {
			done, err := abandon(sweep, model.AssistAbandonTTL)
			if err != nil {
				return abandoned, err
			}
			if done || released[hold.Pod] {
				continue // a hold released meanwhile is running work: not counted
			}
		}
		holds = append(holds, sweep)
	}
	controller.forgetAssistHolds(seen)
	excess := len(holds) - assist.MaxConcurrentHolds
	if excess <= 0 {
		return abandoned, nil
	}
	sort.Slice(holds, func(i, j int) bool { return holds[i].hold.HeldSince.After(holds[j].hold.HeldSince) })
	for _, sweep := range holds {
		if excess == 0 {
			break
		}
		if !assistVictimDue(model.AssistAbandonCap, activity[sweep.hold.Pod], assist.HoldTTL, sweep.hold, now) {
			continue
		}
		done, err := abandon(sweep, model.AssistAbandonCap)
		if err != nil {
			return abandoned, err
		}
		if done {
			excess--
		}
	}
	return abandoned, nil
}

// assistVictimDue applies the sweep rule that chose a victim to one
// activity reading: a released hold is never due; a TTL victim is due when
// its hold TTL has run out; a cap victim is due unless it was worked inside
// model.AssistActivityWindow.
func assistVictimDue(reason string, activity model.AssistPodActivity, ttl time.Duration, hold model.AssistHold, now time.Time) bool {
	if activity.Released {
		return false
	}
	if reason == model.AssistAbandonCap {
		return activity.LastAction.IsZero() || now.Sub(activity.LastAction) >= model.AssistActivityWindow
	}
	return !assistHoldExpiry(hold, activity, ttl).After(now)
}

// assistHoldExpiry is model.AssistHoldExpiry, the rule run_hold_list reports
// as abandon_at.
func assistHoldExpiry(hold model.AssistHold, activity model.AssistPodActivity, ttl time.Duration) time.Time {
	return model.AssistHoldExpiry(hold, activity, ttl)
}

// assistEvictedOf describes a held attempt (parked or continued) whose Pod
// the kubelet evicted: the hold annotation and pause, a retry node, phase
// Failed with reason Evicted. HeldSince is when its main container started,
// if it did.
func assistEvictedOf(pod *corev1.Pod) (model.AssistHold, bool) {
	if !AssistHoldPod(pod) || pod.Status.Phase != corev1.PodFailed || pod.Status.Reason != "Evicted" {
		return model.AssistHold{}, false
	}
	hold := model.AssistHold{Pod: pod.Name, Node: pod.Annotations[argoNodeNameAnnotation]}
	match := retryAttemptSuffix.FindStringSubmatch(hold.Node)
	if match == nil {
		return model.AssistHold{}, false
	}
	if hold.Attempt, _ = strconv.Atoi(match[1]); hold.Attempt < 1 {
		return model.AssistHold{}, false
	}
	for _, container := range pod.Spec.Containers {
		if container.Name != mainContainerName {
			continue
		}
		for _, variable := range container.Env {
			if variable.Name == "OBERTH_STEP" {
				hold.Step = variable.Value
			}
		}
	}
	if hold.Step == "" {
		hold.Step = nodeStepName(pod.Labels[argoWorkflowLabel], hold.Node)
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != mainContainerName {
			continue
		}
		switch {
		case status.State.Terminated != nil && !status.State.Terminated.StartedAt.IsZero():
			hold.HeldSince = status.State.Terminated.StartedAt.Time
		case status.State.Running != nil:
			hold.HeldSince = status.State.Running.StartedAt.Time
		}
	}
	if hold.HeldSince.IsZero() && pod.Status.StartTime != nil {
		hold.HeldSince = pod.Status.StartTime.Time
	}
	return hold, true
}

// assistEviction reports an evicted held attempt once: it is recorded when
// neither the snapshot nor, when set, the fresh record holds its eviction
// yet. The run may already have finished (the Pod stays until its Workflow
// goes), so the fresh record, which reads any run, is what deduplicates.
func (controller *Controller) assistEviction(ctx context.Context, pods []corev1.Pod, pod *corev1.Pod, evicted model.AssistHold, activity map[string]model.AssistPodActivity,
	recheck model.AssistActivityRecheck, now time.Time) (model.AssistAbandon, bool) {
	if activity[evicted.Pod].Evicted {
		return model.AssistAbandon{}, false
	}
	runID := pod.Labels[runLabel]
	if recheck != nil {
		fresh, err := recheck(ctx, runID, evicted.Pod)
		if err != nil || fresh.Evicted {
			return model.AssistAbandon{}, false
		}
	}
	return model.AssistAbandon{RunID: runID, Pod: evicted.Pod, Step: evicted.Step, Reason: model.AssistAbandonEvicted, HeldSince: evicted.HeldSince,
		AbandonedAt: now, FailedAttempt: evicted.Attempt - 1, FailedExitCode: failedAttemptExit(pods, pod, evicted), Eviction: pod.Status.Message}, true
}

type assistSweepHold struct {
	hold       model.AssistHold
	runID      string
	failedExit *int
}

// failedAttemptExit is the main container exit code of the attempt whose
// failure created the hold: the sibling Pod of node "...(N-1)" of the same
// Workflow. Step Pods have no PodGC, so it stays until the Workflow goes.
func failedAttemptExit(pods []corev1.Pod, held *corev1.Pod, hold model.AssistHold) *int {
	base := retryAttemptSuffix.ReplaceAllString(hold.Node, "")
	node := fmt.Sprintf("%s(%d)", base, hold.Attempt-1)
	for index := range pods {
		pod := &pods[index]
		if pod.Labels[argoWorkflowLabel] != held.Labels[argoWorkflowLabel] || pod.Annotations[argoNodeNameAnnotation] != node {
			continue
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == mainContainerName && status.State.Terminated != nil {
				code := int(status.State.Terminated.ExitCode)
				return &code
			}
		}
	}
	return nil
}

// noteAssistHold logs a hold the first time the sweep sees it.
func (controller *Controller) noteAssistHold(sweep assistSweepHold) {
	controller.assistSeen.Lock()
	defer controller.assistSeen.Unlock()
	if controller.assistSeen.pods == nil {
		controller.assistSeen.pods = map[string]bool{}
	}
	if controller.assistSeen.pods[sweep.hold.Pod] {
		return
	}
	controller.assistSeen.pods[sweep.hold.Pod] = true
	log.Printf("assist: hold started run=%s step=%s pod=%s held_since=%s", sweep.runID, sweep.hold.Step, sweep.hold.Pod, sweep.hold.HeldSince.UTC().Format(time.RFC3339))
}

// forgetAssistHolds drops logged holds the sweep no longer sees.
func (controller *Controller) forgetAssistHolds(seen map[string]bool) {
	controller.assistSeen.Lock()
	defer controller.assistSeen.Unlock()
	for pod := range controller.assistSeen.pods {
		if !seen[pod] {
			delete(controller.assistSeen.pods, pod)
		}
	}
}

// boundedBuffer keeps the first limit bytes and counts the rest.
type boundedBuffer struct {
	buffer    bytes.Buffer
	limit     int64
	total     int64
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.total += int64(len(p))
	room := b.limit - int64(b.buffer.Len())
	if room <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > room {
		b.truncated = true
		b.buffer.Write(p[:room])
		return len(p), nil
	}
	b.buffer.Write(p)
	return len(p), nil
}

func (b *boundedBuffer) String() string { return b.buffer.String() }
