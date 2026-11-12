package service

import (
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// queuedDisplayReason uses only native waiting and the submission lifecycle.
func queuedDisplayReason(scheduling *model.RunScheduling) string {
	if scheduling == nil {
		return "in line"
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
	return "in line"
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
		reason = queuedDisplayReason(scheduling)
	}
	run.DisplayReason = reason
}
