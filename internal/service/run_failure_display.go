package service

import (
	"errors"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
)

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
		return "upstream publication failed"
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
