package argojob

import (
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/runprogress"
)

// Called only after matching the exact adopted Workflow, and only for a final,
// unambiguous Pod attempt. A missing process exit is never the synthetic 1 that
// exitCode uses for legacy display/progress. An actual nonzero exit wins over a
// missing Pod or log cursor; unknown/foreign Pod diagnostics grant no authority.
func recordRestartCut(workflow *wfv1.Workflow, node wfv1.NodeStatus, pod *corev1.Pod, step *StepResult) {
	if step.exitEvidenceConflict || step.InfrastructureReason != "" || node.Type != wfv1.NodeTypePod || (node.Phase != wfv1.NodeFailed && node.Phase != wfv1.NodeError) {
		return
	}
	if node.Outputs != nil && node.Outputs.ExitCode != nil && *node.Outputs.ExitCode != "" && !nodeExitCodeKnown(node) {
		return // malformed evidence is not evidence that the exit was lost
	}
	// Node text is never positive evidence, but explicit OOM/deadline evidence
	// must not turn into a restart cut when its Pod has been collected.
	message := strings.ToLower(node.Message)
	if strings.Contains(message, "oomkilled") || strings.Contains(message, "deadline") {
		return
	}
	diagnostic := step.Termination
	if diagnostic != nil && diagnostic.Available && diagnostic.Container != nil {
		t := diagnostic.Container
		// A kubelet termination during an already-requested deletion of this exact
		// Pod is stronger evidence than signal-derived 137/143 alone. Memory-limit
		// OOM and deletion requested after a test failed do not qualify.
		if node.Phase == wfv1.NodeError && (!step.ExitCodeKnown || step.ExitCode == int(t.ExitCode)) && stepPodBound(workflow, node, pod) && pod.Status.Phase == corev1.PodFailed && pod.DeletionTimestamp != nil && !pod.DeletionTimestamp.After(t.FinishedAt) && (t.Signal == 9 || t.Signal == 15) && t.Reason != "OOMKilled" && t.ExitCode == 128+t.Signal {
			step.restartCutReason = "owned Pod was deleted while its container was terminating"
			return
		}
		if t.ExitCode != 0 {
			return
		}
	}
	if step.ExitCodeKnown {
		return
	}
	// Only a complete-snapshot absence or an exact readable Pod lacking its
	// current main termination supplies positive missing-exit evidence. API
	// failures, incomplete lists and nodes that never got a Pod stay ambiguous.
	if diagnostic == nil {
		return
	}
	switch diagnostic.Unavailable {
	case "pod-absent", "termination-unavailable":
	default:
		return
	}
	step.restartCutReason = "adopted Pod attempt has no recorded process exit code"
}

// Run classification is finalized after cleanup adds its verdict. An unrelated
// test failure or a hard resource-cleanup failure must remain red even when a
// parallel step lost its exit evidence during adoption.
func (completion *Completion) classifyRestartCut() {
	completion.Interrupted = false
	if completion.Succeeded {
		return
	}
	var cut *StepResult
	var real *StepResult
	for i := range completion.Steps {
		step := &completion.Steps[i]
		if step.Status == runprogress.StepPassed || step.Status == runprogress.StepSkipped || !terminal(step.Status) {
			continue
		}
		if step.restartCutReason != "" {
			if cut == nil {
				cut = step
			}
			continue
		}
		if step.InfrastructureReason == "" {
			real = step
		}
	}
	if cut == nil {
		return
	}
	if real != nil {
		completion.Infrastructure = false
		completion.FailedBurn, completion.FailedStep = real.Burn, real.Step
		return
	}
	completion.Interrupted = true
	completion.Infrastructure = true
	completion.FailedBurn, completion.FailedStep = "", ""
	completion.Reason = fmt.Sprintf("server restart interrupted %s/%s: %s; %s", cut.Burn, cut.Step, cut.restartCutReason, completion.Reason)
}

// Older journals did not distinguish the synthetic display exit 1. Conservatively
// retain a recorded terminal failure when it binds the exact unretried attempt's
// timestamps; do not let a later incomplete Argo snapshot erase that evidence.
func preserveRecordedExitCodes(completion *Completion, events []runprogress.Event) {
	for i := range completion.Steps {
		step := &completion.Steps[i]
		if step.Attempt != "" || step.AttemptAmbiguous || step.StartedAt == nil || step.FinishedAt == nil {
			continue
		}
		for _, event := range events {
			if event.Validate() != nil || event.Burn != step.Burn || event.Step != step.Step || event.Status != runprogress.StepFailed || event.ExitCode <= 0 || event.StartedAt == nil || event.FinishedAt == nil || !event.StartedAt.Equal(*step.StartedAt) || !event.FinishedAt.Equal(*step.FinishedAt) {
				continue
			}
			if step.ExitCodeKnown && step.ExitCode != event.ExitCode {
				step.exitEvidenceConflict = true
				continue
			}
			step.ExitCode, step.ExitCodeKnown = event.ExitCode, true
		}
	}
}

// The settling GET must not erase a process failure already observed on the
// exact same node/attempt. A later retry has a different ID or timestamps.
func preserveObservedExitCodes(completion *Completion, observed *wfv1.Workflow) {
	known := make(map[string]StepResult)
	for _, step := range StepResults(observed) {
		if step.ExitCodeKnown && step.ExitCode > 0 && step.Status == runprogress.StepFailed {
			known[step.NodeID] = step
		}
	}
	for i := range completion.Steps {
		step := &completion.Steps[i]
		prior, ok := known[step.NodeID]
		if !ok || prior.Burn != step.Burn || prior.Step != step.Step || prior.Attempt != step.Attempt || prior.StartedAt == nil || prior.FinishedAt == nil || step.StartedAt == nil || step.FinishedAt == nil || !prior.StartedAt.Equal(*step.StartedAt) || !prior.FinishedAt.Equal(*step.FinishedAt) {
			continue
		}
		if step.ExitCodeKnown && step.ExitCode != prior.ExitCode {
			step.exitEvidenceConflict = true
			continue
		}
		step.ExitCode, step.ExitCodeKnown = prior.ExitCode, true
	}
}
