package service

import (
	"github.com/oberthci/oberth/internal/stateport"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/stepmodel"
)

const nativeDetailRestricted = "native detail restricted"

type nativeDescription struct{ reason, detail string }

func schedulingDescription(cause string) nativeDescription {
	switch cause {
	case "slot":
		return nativeDescription{"waiting for a run slot", "The native controller postponed this Workflow at its run cap."}
	case "lock":
		return nativeDescription{"waiting for a lock", "Native synchronization is waiting. Run oberth lock status for scoped holders."}
	case "node":
		return nativeDescription{"waiting for a node", "The scheduler reports that this Pod cannot currently be placed."}
	case "quota_wait":
		return nativeDescription{"waiting for quota", "Native admission refused the requested quota."}
	case "quota":
		return nativeDescription{"quota exceeded", "Native admission refused the requested quota."}
	}
	return startupDescription(cause)
}

func startupDescription(cause string) nativeDescription {
	switch cause {
	case "image":
		return nativeDescription{"image pull failed", "A required container image could not be started."}
	case "startup":
		return nativeDescription{"starting containers", "The Pod is waiting for its containers to start."}
	case "disk_pressure":
		return nativeDescription{"node disk pressure", "The assigned Pod has relevant native disk-pressure evidence."}
	case "memory_pressure":
		return nativeDescription{"node memory pressure", "The assigned Pod has relevant native memory-pressure evidence."}
	case "pid_pressure":
		return nativeDescription{"node process pressure", "The assigned Pod has relevant native process-pressure evidence."}
	}
	return infrastructureDescription(cause)
}

func infrastructureDescription(cause string) nativeDescription {
	switch cause {
	case "source":
		return nativeDescription{"source delivery failed", "Source delivery did not complete for this admitted step."}
	case "secret":
		return nativeDescription{"secret delivery failed", "A required secret could not be delivered to this admitted step."}
	case "deadline":
		return nativeDescription{"step timed out", "The native step exceeded its deadline."}
	case "init":
		return nativeDescription{"init container failed", "An init container failed before the authored containers could start."}
	case "oom":
		return nativeDescription{"out of memory", "The container was terminated for exceeding available memory."}
	}
	return nativeDescription{}
}

// Public detail is rebuilt from typed, allowlisted facts for every caller.
// Neither an unscoped actor nor an opaque retained string permits raw disclosure.
func projectNativeSummary(v *stepmodel.RunSummary) {
	reason := safeNativeReason(v.DisplayReason)
	v.DisplayReason = reason
	if v.Status == stepmodel.RunPassed && stepmodel.NativeWait(v.DisplayReason) {
		v.DisplayReason, v.Diagnostic = "", nil
	}
	v.Detail, v.Diagnostic = projectNativeDetail(v.Detail, v.Diagnostic)
	for i := range v.Steps {
		step := &v.Steps[i]
		reason := safeNativeReason(step.DisplayReason)
		step.DisplayReason = reason
		if step.Status == stepmodel.StepUnknown && step.DisplayReason == "" {
			step.DisplayReason = reasonUnknown
		}
		if step.Status == stepmodel.StepPassed && stepmodel.NativeWait(step.DisplayReason) {
			step.DisplayReason, step.Diagnostic = "", nil
		}
		step.Detail, step.Diagnostic = projectNativeDetail(step.Detail, step.Diagnostic)
		if step.Diagnostic == nil {
			step.Logs = nil
			for j := range step.Attempts {
				step.Attempts[j].NodeID, step.Attempts[j].PodName = "", ""
			}
		}
	}
	if v.Failure != nil {
		v.Failure.NodeID = ""
		v.Failure.Retry.ReasonCode = ""
		if v.Failure.Retry.Detail != "" {
			v.Failure.Retry.Detail = nativeDetailRestricted
		}
	}
}

func projectNativeDetail(opaque string, diagnostic *stepmodel.NativeDiagnostic) (string, *stepmodel.NativeDiagnostic) {
	if diagnostic == nil || !slices.Contains([]string{"workflow", "node", "pod"}, diagnostic.Source) {
		return restrictedNativeDetail(opaque, diagnostic)
	}
	if diagnostic.Cause == "" {
		detail := ""
		if opaque != "" {
			detail = nativeDetailRestricted
		}
		return detail, &stepmodel.NativeDiagnostic{Source: diagnostic.Source}
	}
	description := schedulingDescription(diagnostic.Cause)
	if description.detail == "" {
		return restrictedNativeDetail(opaque, diagnostic)
	}
	copy := &stepmodel.NativeDiagnostic{Cause: diagnostic.Cause, Source: diagnostic.Source}
	if diagnostic.TransitionAt != nil {
		copy.TransitionAt = new(*diagnostic.TransitionAt)
		description.detail += "\nReported at " + diagnostic.TransitionAt.UTC().Format(time.RFC3339) + "."
	}
	return nativeConstraintDetail(description.detail, copy, diagnostic.Constraints)
}

func restrictedNativeDetail(opaque string, diagnostic *stepmodel.NativeDiagnostic) (string, *stepmodel.NativeDiagnostic) {
	if opaque != "" || diagnostic != nil {
		return nativeDetailRestricted, nil
	}
	return "", nil
}

func nativeConstraintDetail(detail string, copy *stepmodel.NativeDiagnostic, constraints []string) (string, *stepmodel.NativeDiagnostic) {
	if copy.Cause != "node" {
		return detail, copy
	}
	for _, constraint := range constraints {
		if explanation := schedulingConstraint(constraint); explanation != "" && !slices.Contains(copy.Constraints, constraint) {
			copy.Constraints = append(copy.Constraints, constraint)
			detail += "\n" + explanation
		}
	}
	return detail, copy
}

func schedulingConstraint(constraint string) string {
	switch constraint {
	case "cpu":
		return "The scheduler reports insufficient CPU."
	case "memory":
		return "The scheduler reports insufficient memory."
	case "taint":
		return "The scheduler reports an untolerated taint."
	case "affinity":
		return "The scheduler reports an affinity constraint."
	case "volume":
		return "The scheduler reports a volume constraint."
	}
	return ""
}

func safeNativeReason(reason string) string {
	if strings.HasPrefix(reason, "waiting for sync limit ") || strings.HasPrefix(reason, "waiting for mutex ") {
		return "waiting for a lock"
	}
	if catalogueReason(reason) {
		return reason
	}
	if text, found := strings.CutPrefix(reason, "exited with code "); found {
		if code, err := strconv.ParseInt(text, 10, 32); err == nil {
			return "exited with code " + strconv.FormatInt(code, 10)
		}
	}
	return ""
}

func catalogueReason(reason string) bool {
	switch reason {
	case "", "waiting for a run slot", "waiting for a lock", "waiting for a node", "waiting for quota", "quota exceeded",
		"image pull failed", "starting containers", "node disk pressure", "node memory pressure", "node process pressure",
		"held for debugging", "waiting to start", "not reached", "step failed", "Pod refused by the cluster", "workflow was deleted",
		"native observation failed", "failed before starting", "out of memory", "step timed out", "source delivery failed",
		"secret delivery failed", "delivery evidence unavailable", "init container failed", "node shut down", "Pod disrupted",
		"Pod evicted", "stopped", "skipped by condition", "omitted", "unknown", "passed", "running", "queued", "failed", "cancelled":
		return true
	case reasonQueued, reasonRunning, reasonPassed, reasonFailed, reasonCancelled, reasonUnknown, reasonResumed,
		reasonAssistedRun, reasonPromotionUnavailable, reasonPromotionEvidenceInvalid:
		return true
	case stateport.ReasonRestart, stateport.ReasonBranchDeleted, stateport.ReasonAdmission, stateport.ReasonCreateRefused,
		stateport.ReasonRetentionRefused, stateport.ReasonRestartAbsent, stateport.ReasonWorkflowOwner:
		return true
	}
	return false
}
