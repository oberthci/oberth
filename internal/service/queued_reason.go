package service

import (
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
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
		if qualifier := queueBlockerReason(blocker); qualifier != "" {
			reason = qualifier
		}
	}
	return reason
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

func waitingReason(a *model.AdmissionObservation) (reason string) {
	defer func() { reason = displayFailureText(reason) }()
	blocker := a.BlockedBy
	if !a.HeadOfQueue {
		if reason := queueBlockerReason(blocker); reason != "" {
			return reason
		}
	}
	if a.DiskFloor != nil && a.DiskFloor.BelowFloor {
		return fmt.Sprintf("waiting for disk space (%s free)", bytesText(a.DiskFloor.FreeBytes))
	}
	if a.HeadOfQueue {
		return "next in line"
	}
	return queuePositionReason(int64(a.Position))
}

func queueBlockerReason(blocker *model.AdmissionBlocker) string {
	if blocker != nil && blocker.Repo != "" && blocker.Ref != "" {
		return displayFailureText("behind " + blocker.Repo + "/" + blocker.Ref)
	}
	return ""
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
