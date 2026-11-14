package service

import (
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// queuedDisplayReason uses only native waiting and the submission lifecycle.
func queuedDisplayReason(scheduling *model.RunScheduling) string {
	if scheduling == nil {
		return "starting"
	}
	if scheduling.Execution != nil && lockWaitReason(scheduling.Execution) != "" {
		return lockWaitReason(scheduling.Execution)
	}
	if scheduling.Execution != nil && scheduling.Execution.Message != "" {
		return displayFailureText(scheduling.Execution.Message)
	}
	switch scheduling.State {
	case "infra-pending":
		return "workflow create uncertain, cleanup pending"
	case "preparing", "startup":
		return "starting"
	}
	return "starting"
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

// applyQueuedDisplay uses the native Pending observation for the queue reason.
func applyQueuedDisplay(run *model.Run, scheduling *model.RunScheduling, reason string) {
	if run.DisplayStatus != model.DisplayQueued || scheduling == nil {
		return
	}
	if reason == "" {
		reason = queuedDisplayReason(scheduling)
	}
	run.DisplayReason = reason
}
