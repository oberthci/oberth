package service

import (
	"errors"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
)

func displayFailureText(message string) string {
	text := strings.Join(strings.Fields(message), " ")
	const limit = 320
	chars := []rune(text)
	if len(chars) > limit {
		return string(chars[:limit]) + "…"
	}
	return text
}

func failedRunDisplayReason(run model.Run) string {
	message := displayFailureText(run.Error)
	if run.Phase == "publishing" || pushretry.PublicationFailure(run.Error) {
		lower := strings.ToLower(run.Error)
		for _, class := range []string{"non-fast-forward", "permission denied", "authentication failed", "access denied", "pre-receive hook declined"} {
			if strings.Contains(lower, class) && !pushretry.Transient(errors.New(run.Error)) && !pushretry.PublicationFailure(run.Error) {
				return "upstream rejected: " + class
			}
		}
		prefix := "upstream publication failed"
		if pushretry.PublicationFailure(run.Error) || pushretry.Transient(errors.New(run.Error)) {
			prefix += ": transient"
		}
		if message != "" {
			return prefix + ": " + message
		}
		return prefix
	}
	phase := strings.TrimSpace(run.Phase)
	if phase == "" {
		phase = "run"
	}
	reason := phase + " failed"
	if run.FailedStep != "" {
		reason += ": " + run.FailedStep
	}
	if message != "" {
		reason += ": " + message
	}
	return reason
}
