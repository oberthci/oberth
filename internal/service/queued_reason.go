package service

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// queuedDisplayReason is the display_reason of a run that has not started:
// what it waits for, in plain words, next to the one grey queued status.
// The raw scheduling state (queued, preparing, waiting, admitted, preflight)
// stays machine-readable in Scheduling.state; this line is what people read.
// The observation passed in is already redacted for the viewer's scope, so a
// scoped viewer gets "waiting for capacity" where an admin sees the numbers.
func queuedDisplayReason(scheduling *model.RunScheduling, blocker *model.AdmissionBlocker) string {
	if scheduling == nil {
		return "next in line"
	}
	var reason string
	switch scheduling.State {
	case "infra-pending":
		return "workflow create uncertain, cleanup pending"
	case "queued":
		reason = "next in line"
		if scheduling.QueuePosition > 1 {
			reason = fmt.Sprintf("position %d in line", scheduling.QueuePosition)
		}
	case "preparing":
		return "starting"
	case "preflight":
		return "KVM preflight"
	case "waiting":
		if scheduling.Admission == nil {
			return "waiting for capacity"
		}
		if scheduling.Admission.State == "admitted" {
			return "starting"
		}
		return waitingReason(scheduling.Admission)
	default:
		return "next in line"
	}
	return displayFailureText(withWaitQualifier(reason, nil, blocker))
}

func waitingReason(a *model.AdmissionObservation) string {
	reason := "waiting for capacity"
	switch {
	case a.KubeVirtCapacity:
		reason = "waiting for KubeVirt capacity"
	case a.VMCapacity:
		reason = "waiting for VM capacity"
	case a.DiskFloor != nil && a.DiskFloor.BelowFloor:
		reason = fmt.Sprintf("waiting for disk space (%s free)", bytesText(a.DiskFloor.FreeBytes))
	case a.GroupLimit > 0 && a.GroupBlocked, a.GroupHolder != "", a.GroupAhead != "":
		reason = "group " + a.Group + " busy"
	case a.MaxRuns > 0 && a.Runs >= a.MaxRuns:
		reason = fmt.Sprintf("%d runs already running (limit %d)", a.Runs, a.MaxRuns)
	case a.BlockedBy != nil && a.BlockedBy.RunCeiling:
		reason = "waiting for a run slot"
	case a.DeclaredResources != nil:
		reason = budgetReason(a.NumericBudget, a.DeclaredResources)
	case a.Size != "":
		reason = fmt.Sprintf("waiting for a slot (size %s)", a.Size)
	}
	blocker := a.BlockedBy
	if a.HeadOfQueue {
		blocker = nil
	}
	pressure := a.Pressure
	if pressure == nil && a.BlockedBy != nil {
		pressure = a.BlockedBy.Pressure
	}
	return displayFailureText(withWaitQualifier(reason, pressure, blocker))
}

func withWaitQualifier(reason string, pressure *model.PressureDeferral, blocker *model.AdmissionBlocker) string {
	if pressure == nil && blocker != nil {
		pressure = blocker.Pressure
	}
	if pressure != nil {
		if pressure.Resource == "io" || pressure.Resource == "cpu+io" {
			return reason + " · host IO pressure"
		}
		if pressure.Resource == "cpu" {
			return reason + " · host CPU pressure"
		}
	}
	// A redacted blocker has no identity; never invent a hidden repo/ref.
	if blocker != nil && blocker.Repo != "" && blocker.Ref != "" {
		return reason + " · behind " + blocker.Repo + "/" + blocker.Ref
	}
	return reason
}

func budgetReason(_ *model.NumericBudget, resources *model.DeclaredResources) string {
	return fmt.Sprintf("waiting for %s CPU, %s", cpuText(resources.CPUMilli), bytesText(resources.MemoryBytes))
}

// cpuText renders millicores as cores: 12500 -> "12.5", 40000 -> "40".
func cpuText(milli int64) string {
	return strings.TrimSuffix(strings.TrimRight(strconv.FormatFloat(float64(milli)/1000, 'f', 1, 64), "0"), ".")
}

// bytesText renders bytes in the largest binary unit with one decimal.
func bytesText(bytes int64) string {
	const unit = 1024
	value := float64(bytes)
	for _, suffix := range []string{"B", "KiB", "MiB", "GiB", "TiB"} {
		if value < unit || suffix == "TiB" {
			if suffix == "B" {
				return fmt.Sprintf("%d B", bytes)
			}
			return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.1f", value), "0"), ".") + " " + suffix
		}
		value /= unit
	}
	return fmt.Sprintf("%d B", bytes)
}

// applyQueuedDisplay sets display_reason of a run that has not started from
// its scheduling observation. It is the only place a queued run's reason
// comes from, so every surface (list, detail, live log, status, active
// view) reads the same line.
func applyQueuedDisplay(run *model.Run, scheduling *model.RunScheduling, reason string) {
	if run.DisplayStatus != model.DisplayQueued || scheduling == nil {
		return
	}
	if reason == "" {
		reason = queuedDisplayReason(scheduling, nil)
	}
	run.DisplayReason = displayFailureText(joinRunMessage(priorityPrefix(*run), reason))
}
