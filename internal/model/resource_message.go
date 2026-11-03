package model

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// ResourceCoverage is optional, immutable submission metadata in the existing
// workflow timing JSON. Old runs have unknown coverage, rather than zero.
type ResourceCoverage struct {
	PodDemand  *argoworkflow.PodDemand   `json:"pod_demand,omitempty"`
	Unknown    bool                      `json:"unknown,omitempty"`
	Fallback   ResourceAdmissionFallback `json:"fallback,omitempty"`
	Declared   int                       `json:"declared"`
	Undeclared int                       `json:"undeclared"`
}

func (c ResourceCoverage) Valid() bool {
	return (c.PodDemand == nil || c.PodDemand.Valid()) && c.Fallback.Valid() && (!c.Unknown || c.Declared == 0 && c.Undeclared == 0) && c.Declared >= 0 && c.Undeclared >= 0 && c.Declared <= 512 && c.Undeclared <= 512-c.Declared
}
func (c ResourceCoverage) Message(runID string) string {
	if !c.Valid() || !diagnosticRun.MatchString(runID) {
		return ""
	}
	message := ""
	if c.Undeclared > 0 {
		message = fmt.Sprintf("%d steps have undeclared CPU/memory requests; inspect oberth run resources --run %s --include-failed.", c.Undeclared, runID)
	}
	if c.PodDemand != nil {
		message = strings.TrimSpace(message + " " + c.PodDemand.ConcurrencyMessage())
	}
	return strings.TrimSpace(message + " " + c.Fallback.Message())
}

func resourceBytes(n *int64) string {
	if n == nil || *n < 0 {
		return "unknown"
	}
	return resource.NewQuantity(*n, resource.BinarySI).String()
}

// ResourceFailureMessage uses only bound pod-spec diagnostics and external
// sampler values. Self-reported peaks and raw termination messages are ignored.
// Only failed final steps participate; a successful retry keeps its verdict.
func ResourceFailureMessage(runID string, steps []StepResult, usage []StepResourceUsage, deadline bool) string {
	if !diagnosticRun.MatchString(runID) {
		return ""
	}
	failed := 0
	for _, step := range steps {
		if step.Status == StepFailed || step.Status == StepTimedOut {
			failed++
		}
	}
	for _, step := range steps {
		if step.Status != StepFailed && step.Status != StepTimedOut {
			continue
		}
		if !diagnosticName.MatchString(step.Stage) || !diagnosticName.MatchString(step.Step) {
			continue
		}
		var c *StepTermination
		if d := step.Termination; d != nil && d.Available && d.Validate() == nil && d.Container != nil && d.Container.RunID == runID {
			c = d.Container
		}
		oom := c != nil && c.Reason == "OOMKilled"
		timed := step.Status == StepTimedOut || (c != nil && c.PodReason == "DeadlineExceeded") || (deadline && failed == 1)
		if !oom && !timed {
			continue
		}
		var peak *int64
		if c != nil {
			for _, v := range usage {
				if v.Source != "kubelet-cadvisor" || v.Saturated || v.RunID != runID || v.WorkflowUID != c.WorkflowUID || v.PodUID != c.PodUID || v.ContainerID != c.ContainerID || v.ContainerName != c.ContainerName || v.RestartCount != c.RestartCount {
					continue
				}
				if v.MemoryPeakBytes != nil && *v.MemoryPeakBytes >= 0 && (peak == nil || *v.MemoryPeakBytes > *peak) {
					n := *v.MemoryPeakBytes
					peak = &n
				}
			}
		}
		command := "oberth run resources --run " + runID + " --include-failed"
		if oom {
			return fmt.Sprintf("OOM kill at %s/%s: memory limit %s; sampled memory peak %s (lower bound). Inspect %s; change resources in git.", step.Stage, step.Step, resourceBytes(c.MemoryLimitBytes), resourceBytes(peak), command)
		}
		limit := "unknown"
		runtime := "unknown"
		if c != nil {
			if c.ActiveDeadlineSeconds != nil {
				limit = fmt.Sprintf("%ds", *c.ActiveDeadlineSeconds)
			}
			if !c.StartedAt.IsZero() && !c.FinishedAt.Before(c.StartedAt) {
				runtime = c.FinishedAt.Sub(c.StartedAt).String()
			}
		}
		return fmt.Sprintf("%s Step %s/%s: deadline limit %s; observed runtime %s; sampled memory peak %s (lower bound). Inspect %s; change deadlines in git.", ExecutionDeadlineMessage, step.Stage, step.Step, limit, runtime, resourceBytes(peak), command)
	}
	if deadline {
		return fmt.Sprintf("%s Step unknown: deadline limit unknown; sampled memory peak unknown. Inspect oberth run resources --run %s --include-failed; change deadlines in git.", ExecutionDeadlineMessage, runID)
	}
	return ""
}

func IsResourceFailureMessage(message string) bool {
	return (strings.HasPrefix(message, "OOM kill at ") || strings.HasPrefix(message, ExecutionDeadlineMessage+" Step ")) && strings.Contains(message, "Inspect oberth run resources --run ")
}
