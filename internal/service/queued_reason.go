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
		return "awaiting scheduler"
	}
	admission := scheduling.Admission
	switch scheduling.State {
	case "infra-pending":
		return "workflow create outcome uncertain; cleanup pending"
	case "queued":
		reason := "in line"
		switch {
		case scheduling.QueuePosition == 1:
			reason = "next in line"
		case scheduling.QueuePosition > 1:
			reason = fmt.Sprintf("position %d in line", scheduling.QueuePosition)
		}
		if blocker != nil {
			reason += ", " + behindBlocker(blocker)
		}
		return reason
	case "preparing":
		return "preparing to run"
	case "preflight":
		return "KVM preflight running"
	case "waiting":
		switch {
		case admission == nil:
			// No reservation is known for the claimed run (just claimed,
			// or the gate is being rebuilt after a restart): that is not
			// proof of admission.
			return "waiting for admission"
		case admission.State == "admitted":
			return "admitted, starting the workflow"
		default:
			return waitingReason(admission)
		}
	default:
		return "awaiting scheduler"
	}
}

// waitingReason names the one thing a claimed run waits for at the gate.
func waitingReason(admission *model.AdmissionObservation) string {
	switch {
	case admission.KubeVirtCapacity:
		return "waiting for KubeVirt capacity (repository VM grant)"
	case admission.VMCapacity:
		return "waiting for trusted VM capacity"
	case admission.DiskFloor != nil && admission.DiskFloor.BelowFloor:
		return fmt.Sprintf("waiting for disk space (%s free, floor %s)", bytesText(admission.DiskFloor.FreeBytes), bytesText(admission.DiskFloor.FloorBytes))
	case admission.GroupHolder != "":
		return fmt.Sprintf("concurrency group %s busy (held by run %s)", admission.Group, shortRunID(admission.GroupHolder))
	case admission.GroupAhead != "":
		return fmt.Sprintf("concurrency group %s busy (run %s is ahead)", admission.Group, shortRunID(admission.GroupAhead))
	}
	blocker := admission.BlockedBy
	pressure := admission.Pressure
	if pressure == nil && blocker != nil {
		pressure = blocker.Pressure
	}
	behind := ""
	if blocker != nil && !admission.HeadOfQueue {
		behind = ", " + behindBlocker(blocker)
		if admission.Pressure == nil && blocker.Pressure != nil || blocker.RunCeiling {
			// The head's cause is this run's cause too; name it once.
			behind = ", behind " + blocker.Repo + "/" + blocker.Ref
		}
	}
	if pressure != nil {
		return pressureReason(pressure) + behind
	}
	reason := "waiting for capacity"
	switch {
	case blocker != nil && blocker.RunCeiling && admission.MaxRuns > 0:
		reason = fmt.Sprintf("%d runs already running (limit %d)", admission.Runs, admission.MaxRuns)
	case blocker != nil && blocker.RunCeiling:
		reason = "waiting for a run slot"
	case admission.NumericBudget != nil && admission.DeclaredResources != nil:
		reason = budgetReason(admission.NumericBudget, admission.DeclaredResources)
	case admission.Capacity > 0 && admission.Weight > 0:
		reason = fmt.Sprintf("waiting for capacity (size %s, %d of %d weight used)", admission.Size, admission.Used, admission.Capacity)
	}
	if admission.HeadOfQueue && blocker != nil {
		switch {
		case blocker.Backfilling:
			reason += "; smaller runs run meanwhile"
		case blocker.Draining:
			reason += "; host draining for it"
		}
	}
	return reason + behind
}

// budgetReason names the budget the declared reservation does not fit, with
// what is free of it: "waiting for CPU budget (12.5 of 40 CPU free)".
func budgetReason(budget *model.NumericBudget, declared *model.DeclaredResources) string {
	freeCPU, freeMemory := budget.CPUBudgetMilli-budget.CPUUsedMilli, budget.MemoryBudgetBytes-budget.MemoryUsedBytes
	switch {
	case declared.CPUMilli > freeCPU:
		return fmt.Sprintf("waiting for CPU budget (%s of %s CPU free)", cpuText(freeCPU), cpuText(budget.CPUBudgetMilli))
	case declared.MemoryBytes > freeMemory:
		return fmt.Sprintf("waiting for memory budget (%s of %s free)", bytesText(freeMemory), bytesText(budget.MemoryBudgetBytes))
	default:
		return fmt.Sprintf("waiting for capacity (%s of %s CPU, %s of %s memory free)", cpuText(freeCPU), cpuText(budget.CPUBudgetMilli), bytesText(freeMemory), bytesText(budget.MemoryBudgetBytes))
	}
}

// pressureReason reads "host IO pressure (42%)"; a scoped viewer sees no number.
func pressureReason(pressure *model.PressureDeferral) string {
	reason := "host " + pressureName(pressure.Resource) + " pressure"
	switch {
	case pressure.Redacted:
		return reason
	case pressure.Avg10Gate:
		return fmt.Sprintf("%s (CPU %.0f%%, IO %.0f%%)", reason, pressure.CPUAvg10, pressure.IOAvg10)
	default:
		return fmt.Sprintf("%s (%.0f%%)", reason, pressure.Avg60)
	}
}

func pressureName(resource string) string {
	switch resource {
	case "cpu":
		return "CPU"
	case "io":
		return "IO"
	case "cpu+io":
		return "CPU and IO"
	default:
		return resource
	}
}

// behindBlocker names the queue head every later run waits behind, in the
// blocker's own terms: "behind org/repo/main (needs the whole host)".
func behindBlocker(blocker *model.AdmissionBlocker) string {
	why := ""
	switch {
	case blocker.FullDrain:
		why = "needs the whole host"
	case blocker.Pressure != nil:
		why = pressureReason(blocker.Pressure)
	case blocker.RunCeiling:
		why = "waiting for a run slot"
	case blocker.DiskFloor:
		why = "waiting for disk space"
	case blocker.NumericBudget != nil && blocker.DeclaredResources != nil:
		why = fmt.Sprintf("needs %s CPU, %s memory", cpuText(blocker.DeclaredResources.CPUMilli), bytesText(blocker.DeclaredResources.MemoryBytes))
	case blocker.Size != "" && blocker.Capacity > 0:
		why = fmt.Sprintf("size %s, needs %d weight, %d of %d used", blocker.Size, blocker.Weight, blocker.Used, blocker.Capacity)
	case blocker.Size != "":
		why = "size " + blocker.Size
	}
	if why == "" {
		return "behind " + blocker.Repo + "/" + blocker.Ref
	}
	return "behind " + blocker.Repo + "/" + blocker.Ref + " (" + why + ")"
}

// cpuText renders millicores as cores: 12500 -> "12.5", 40000 -> "40".
func cpuText(milli int64) string {
	return strings.TrimSuffix(strings.TrimRight(strconv.FormatFloat(float64(milli)/1000, 'f', 2, 64), "0"), ".")
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
