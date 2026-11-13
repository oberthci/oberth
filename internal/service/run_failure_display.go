package service

import (
	"errors"
	"regexp"

	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
)

const validationRefusalPrefix = "validation refused: "

// The wire reason is one bounded line; Error and Reason retain detail evidence.
func displayFailureText(message string) string {
	text := strings.Join(strings.Fields(message), " ")
	chars := []rune(text)
	if len(chars) > 60 {
		return string(chars[:59]) + "…"
	}
	return text
}

func failedRunDisplayReason(run model.Run) string {
	// A step whose held retry the sweep abandoned failed at its first
	// attempt; the assisted fragment (assistDisplayReason) names that step,
	// the attempt's exit code and the hold. The held Pod's runtime and
	// deadline (budget plus two hold TTLs) say nothing about the step, so no
	// generic deadline or step wording is added in front of it.
	if step := run.FailedStep; step != "" && run.Assist != nil && run.Assist.SweepAbandoned(step) {
		return ""
	}
	if reason := evictionDisplayReason(run); reason != "" {
		return reason
	}
	if reason := nodeRebootDisplayReason(run); reason != "" {
		return reason
	}
	if run.FailureCause != "" {
		return listFailureCause(run)
	}
	if run.Phase == "publishing" || pushretry.PublicationFailure(run.Error) {
		lower := strings.ToLower(run.Error)
		for _, class := range []string{"non-fast-forward", "permission denied", "authentication failed", "access denied", "pre-receive hook declined"} {
			if strings.Contains(lower, class) && !pushretry.Transient(errors.New(run.Error)) && !pushretry.PublicationFailure(run.Error) {
				return "upstream rejected: " + class
			}
		}
		return "steps passed; upstream publication failed"
	}
	step := run.FailedStep
	if step == "" {
		step = run.FailedStage
	}
	if step != "" {
		reason := step + " failed"
		if model.IsResourceFailureMessage(run.Error) && strings.HasPrefix(run.Error, "OOM kill at ") {
			return reason + " · OOM"
		}
		if model.IsExecutionDeadlineReason(run.Error) {
			return reason + " · deadline"
		}
		return reason
	}
	if model.IsResourceFailureMessage(run.Error) && strings.HasPrefix(run.Error, "OOM kill at ") {
		return "infrastructure error: OOM"
	}
	if model.IsExecutionDeadlineReason(run.Error) {
		return "infrastructure error: deadline exceeded"
	}
	if upstreamConfigurationFailure(run.Error) {
		return "upstream configuration error: " + run.Error
	}
	phase := strings.TrimSpace(run.Phase)
	if phase == "" {
		if run.Error == "" {
			return "failed"
		}
		phase = "infrastructure"
	}
	if run.Error == "" {
		return phase + " error"
	}
	return phase + " error: " + run.Error
}

func nodeRebootDisplayReason(run model.Run) string {
	if !model.IsInfrastructureRunFailure(run.Status, run.Phase, run.FailedStep) || !model.IsNodeRebootReason(run.Error) || len(run.Error) > 1024 {
		return ""
	}
	return strings.TrimPrefix(run.Error, model.NodeRebootReasonPrefix)
}

// Persisted structured causes retain their full stage/step identity. Project
// only an exact identity-bound prefix into the list, including historical runs;
// arbitrary error text and paths are not parsed or rewritten.
func listFailureCause(run model.Run) string {
	cause := run.FailureCause
	prefix := run.FailedStage + "/" + run.FailedStep + " failed"
	step := run.FailedStep
	if step == "" {
		step = run.FailedStage
	}
	leaf := "failed"
	if step != "" {
		leaf = step + " failed"
	}
	if cause == displayFailureText(prefix) {
		return leaf
	}
	if suffix, ok := strings.CutPrefix(cause, prefix); ok {
		if len([]rune(cause)) == 60 && strings.HasSuffix(suffix, "…") {
			retained := strings.TrimSuffix(suffix, "…")
			for _, kind := range []struct{ label, prefix string }{{" · OOM", " · OOM (limit "}, {" · deadline", " · deadline "}} {
				if strings.HasPrefix(kind.prefix, retained) || strings.HasPrefix(retained, kind.prefix) {
					// Keep the cause only if its whole name survived. A partial
					// word, limit or deadline value cannot supply missing facts.
					if strings.HasPrefix(retained, kind.label) {
						return leaf + kind.label
					}
					return leaf
				}
			}
		}
		if suffix == " · OOM" || strings.HasPrefix(suffix, " · OOM (limit ") || suffix == " · deadline" || strings.HasPrefix(suffix, " · deadline ") {
			return leaf + suffix
		}
	}
	return cause
}

var evictionStorageLimit = regexp.MustCompile(`^Evicted: Pod ephemeral local storage usage exceeds the total limit of containers ([0-9]+(?:[.][0-9]+)?[KMGTPE]i?)`)

func evictionDisplayReason(run model.Run) string {
	if run.Phase != "infrastructure" || run.FailedStep != "" || (run.Error != "Evicted" && !strings.HasPrefix(run.Error, "Evicted: ")) {
		return ""
	}
	if match := evictionStorageLimit.FindStringSubmatch(run.Error); len(match) == 2 {
		if quantity, err := resource.ParseQuantity(match[1]); err == nil {
			return "evicted: pod storage limit " + bytesText(quantity.Value())
		}
	}
	switch {
	case strings.Contains(run.Error, "The node was low on resource: ephemeral-storage."):
		return "evicted: node storage pressure"
	case strings.Contains(run.Error, "The node was low on resource: memory."):
		return "evicted: node memory pressure"
	case strings.Contains(run.Error, "Pod ephemeral local storage usage exceeds"):
		return "evicted: pod storage limit"
	case strings.Contains(run.Error, "local ephemeral storage limit"):
		return "evicted: container storage limit"
	case strings.HasPrefix(run.Error, "Evicted: Usage of EmptyDir volume "):
		return "evicted: temporary volume storage limit"
	default:
		return "evicted by kubelet"
	}
}
