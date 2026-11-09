package argojob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kexec "k8s.io/client-go/util/exec"

	"github.com/oberthci/oberth/internal/model"
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
		ExecTimeout: assist.ExecTimeout, ExecOutputBytes: assist.ExecOutputBytes, PatchMaxBytes: assist.PatchMaxBytes, PatchMaxFiles: assist.PatchMaxFiles}
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
	if match := retryAttemptSuffix.FindStringSubmatch(hold.Node); match != nil {
		hold.Attempt, _ = strconv.Atoi(match[1])
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

// EnforceAssistHoldCap abandons the newest parked attempts above the
// installation cap. Holds already released by their agent are not counted:
// their command is running.
func (controller *Controller) EnforceAssistHoldCap(ctx context.Context, released map[string]bool) ([]string, error) {
	if controller.kube == nil || !controller.config.Assist.Enabled {
		return nil, nil
	}
	cap := controller.config.Assist.Effective().MaxConcurrentHolds
	pods, err := controller.kube.CoreV1().Pods(controller.Namespace()).List(ctx, metav1.ListOptions{LabelSelector: "oberth.ci/trigger=branch"})
	if err != nil {
		return nil, fmt.Errorf("list branch pods: %w", err)
	}
	var holds []model.AssistHold
	for index := range pods.Items {
		if hold, ok := assistHoldOf(&pods.Items[index]); ok && !released[hold.Pod] {
			holds = append(holds, hold)
		}
	}
	if len(holds) <= cap {
		return nil, nil
	}
	sort.Slice(holds, func(i, j int) bool { return holds[i].HeldSince.Before(holds[j].HeldSince) })
	var abandoned []string
	for _, hold := range holds[cap:] {
		if err := controller.kube.CoreV1().Pods(controller.Namespace()).Delete(ctx, hold.Pod, metav1.DeleteOptions{}); err != nil {
			return abandoned, fmt.Errorf("abandon hold %s over cap: %w", hold.Pod, err)
		}
		abandoned = append(abandoned, hold.Pod)
	}
	return abandoned, nil
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
