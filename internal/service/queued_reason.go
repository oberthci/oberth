package service

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// queuedDisplayReason combines this run's pod requests with exactly one blocker.
// Unknown figures are omitted; global counts and other repos are already scoped.
func queuedDisplayReason(scheduling *model.RunScheduling, blocker *model.AdmissionBlocker) string {
	if scheduling == nil {
		return "in line"
	}
	reason := queuePositionReason(scheduling.QueuePosition)
	switch scheduling.State {
	case "infra-pending":
		return "workflow create uncertain, cleanup pending"
	case "preparing":
		return "starting"
	case "preflight":
		return "retired execution preflight"
	case "waiting":
		if scheduling.Admission != nil {
			if scheduling.Admission.State == "admitted" {
				return "starting"
			}
			reason = waitingReason(scheduling.Admission)
		}
	default:
		if qualifier := queueBlockerReason(nil, blocker); qualifier != "" {
			reason = qualifier
		}
	}
	requests := scheduling.Requests
	if requests == nil && scheduling.Admission != nil {
		requests = scheduling.Admission.Requests
	}
	return withQueueRequests(requests, reason)
}

func queuePositionReason(position int64) string {
	if position == 1 {
		return "next in line"
	}
	if position > 1 {
		return fmt.Sprintf("position %d", position)
	}
	return "in line"
}

func withQueueRequests(requests *argoworkflow.PodRequests, reason string) string {
	reason = displayFailureText(reason)
	if requests == nil {
		return reason
	}
	return fmt.Sprintf("needs %s CPU, %s · %s", cpuText(requests.CPUMilli), bytesText(requests.MemoryBytes), reason)
}

func waitingReason(a *model.AdmissionObservation) (reason string) {
	defer func() { reason = displayFailureText(reason) }()
	blocker := a.BlockedBy
	pressure := a.Pressure
	if pressure == nil && blocker != nil {
		pressure = blocker.Pressure
	}
	if reason := queuePressureReason(pressure); reason != "" {
		return reason
	}
	if a.MaxRuns > 0 && a.Runs >= a.MaxRuns {
		return fmt.Sprintf("%d of %d runs running", a.Runs, a.MaxRuns)
	}
	if !a.HeadOfQueue {
		if reason := queueBlockerReason(nil, blocker); reason != "" {
			return reason
		}
	}
	switch {
	case a.DiskFloor != nil && a.DiskFloor.BelowFloor:
		return fmt.Sprintf("waiting for disk space (%s free)", bytesText(a.DiskFloor.FreeBytes))
	case a.GroupBlocked || a.GroupHolder != "" || a.GroupAhead != "":
		return "group " + a.Group + " busy"
	case a.ExclusiveBlocked:
		return "exclusive " + a.ExclusiveClass + " busy"
	}
	if a.HeadOfQueue {
		return "next in line"
	}
	return queuePositionReason(int64(a.Position))
}

func queuePressureReason(pressure *model.PressureDeferral) string {
	if pressure == nil {
		return ""
	}
	switch pressure.Resource {
	case "io", "cpu+io":
		return "host IO busy"
	case "cpu":
		return "host CPU busy"
	}
	return ""
}

func queueBlockerReason(pressure *model.PressureDeferral, blocker *model.AdmissionBlocker) string {
	if pressure == nil && blocker != nil {
		pressure = blocker.Pressure
	}
	if reason := queuePressureReason(pressure); reason != "" {
		return reason
	}
	if blocker != nil && blocker.Repo != "" && blocker.Ref != "" {
		return displayFailureText("behind " + blocker.Repo + "/" + blocker.Ref)
	}
	return ""
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
	run.DisplayReason = reason
}
