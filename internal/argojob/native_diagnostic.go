package argojob

import (
	"encoding/json"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
	"strings"

	"github.com/oberthci/oberth/internal/stepmodel"
)

func workflowDiagnostic(reason, detail string, w *wf.Workflow) *stepmodel.NativeDiagnostic {
	d := nativeDiagnostic(reason, detail, "workflow")
	if d != nil {
		d.WorkflowUID = retainedUID(string(w.UID))
	}
	return d
}

func observationDiagnostic(o Observation, w *wf.Workflow) *stepmodel.NativeDiagnostic {
	for _, step := range o.Steps {
		if step.DisplayReason == o.Reason && step.Diagnostic != nil {
			return step.Diagnostic.Clone()
		}
	}
	return workflowDiagnostic(o.Reason, o.Detail, w)
}

func projectedDiagnostic(in projectionInput, pod *core.Pod, diagnosis diagnosis, reason string) *stepmodel.NativeDiagnostic {
	source := "node"
	if pod != nil {
		source = "pod"
	}
	d := nativeDiagnostic(reason, diagnosis.detail, source)
	if diagnosis.fact != nil {
		d = diagnosis.fact.Clone()
	}
	if d != nil {
		d.WorkflowUID = retainedUID(string(in.w.UID))
		if pod != nil && d.NativeUID == "" {
			d.NativeUID = retainedUID(string(pod.UID))
		}
	}
	return d
}

func retainedUID(uid string) string {
	body, err := json.Marshal(uid)
	if err != nil || len(uid) > 128 || len(body) > 130 {
		return ""
	}
	return uid
}

func nativeDiagnostic(reason, detail, source string) *stepmodel.NativeDiagnostic {
	cause := nativeCause(reason)
	d := &stepmodel.NativeDiagnostic{Cause: cause, Source: source}
	if cause == "node" {
		// Classification selects fixed explanations, never surrounding values.
		text := strings.ToLower(detail[:min(len(detail), 16<<10)])
		for _, constraint := range []struct{ needle, cause string }{
			{"insufficient cpu", "cpu"}, {"insufficient memory", "memory"},
			{"untolerated taint", "taint"}, {"affinity", "affinity"}, {"volume", "volume"},
		} {
			if strings.Contains(text, constraint.needle) {
				d.Constraints = append(d.Constraints, constraint.cause)
			}
		}
	}
	return d
}

func nativeCause(reason string) string {
	switch reason {
	case reasonRunSlot:
		return "slot"
	case reasonNode:
		return "node"
	case reasonImage:
		return "image"
	case reasonStarting:
		return "startup"
	case reasonQuotaWait:
		return "quota_wait"
	case reasonQuota:
		return "quota"

	}
	if reason == reasonLock || strings.HasPrefix(reason, "waiting for sync limit ") || strings.HasPrefix(reason, "waiting for mutex ") {
		return "lock"
	}
	return failureCause(reason)
}

func failureCause(reason string) string {
	switch reason {
	case reasonSource:
		return "source"
	case reasonSecret:
		return "secret"
	case reasonDeadline:
		return "deadline"
	case reasonInit:
		return "init"
	case reasonOOM:
		return "oom"
	}
	return pressureCause(reason)
}

func pressureCause(reason string) string {
	switch reason {
	case reasonDiskPressure:
		return "disk_pressure"
	case reasonMemoryPressure:
		return "memory_pressure"
	case reasonPIDPressure:
		return "pid_pressure"
	}
	return ""
}

func quotaRefusal(message string) bool {
	return strings.Contains(message, "is forbidden: exceeded quota")
}
