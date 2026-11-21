package runlog

import (
	"slices"

	"github.com/oberthci/oberth/internal/publicerr"
)

type selected struct {
	spans          []span
	reason, detail string
	truncated      bool
	follow         bool
	copying        bool
}

func selection(saved index, request ReadRequest) (selected, error) {
	filtered := !unfiltered(request)
	result := selected{reason: saved.Reason, detail: saved.Detail, truncated: saved.Reason != ""}
	if !filtered {
		result.spans = []span{{0, saved.Size}}
	}
	matched := false
	var pending CaptureState
	for _, entry := range saved.Containers {
		if !matchesRequest(entry, request) {
			continue
		}
		matched = true
		result.observe(entry)
		if entry.State != Captured {
			pending = entry.State
			result.explain(entry)
			continue
		}
		if filtered {
			result.spans = append(result.spans, span{entry.Start, entry.End})
		}
		result.explain(entry)
	}
	if filtered && !matched {
		return result, missingSelection(saved, request)
	}
	if len(result.spans) == 0 {
		return result, notStored(stateReason(pending))
	}
	return result, nil
}

func matches(entry ContainerLog, selector Selector) bool {
	if (selector.StepSet || selector.Step != "") && selector.Step != entry.Step {
		return false
	}
	if selector.NodeID != "" && selector.NodeID != entry.NodeID {
		return false
	}
	if selector.Container != "" {
		return selector.Container == entry.Container
	}
	mainOnly := selector.StepSet || selector.Step != "" || selector.NodeID != ""
	return !mainOnly || entry.Main
}

func (r *selected) explain(entry ContainerLog) {
	if entry.Reason == "" {
		return
	}
	if r.reason == "" {
		r.reason = entry.Reason
	} else if r.reason != entry.Reason {
		r.reason = incompleteReason
	}
	if r.detail == "" {
		r.detail = entry.Detail
	} else if entry.Detail != "" {
		r.detail += "\n" + entry.Detail
	}
	r.truncated = true
}

func stateReason(state CaptureState) string {
	switch state {
	case Following:
		return followingReason
	case Copying:
		return copyingReason
	case Interrupted:
		return pendingReason
	default:
		return waitingReason
	}
}

func notStored(reason string) error {
	remedy := pendingRemedy
	if reason == pendingReason {
		remedy = interruptedRemedy
	}
	if reason == purgedReason {
		remedy = purgedRemedy
	}
	return publicerr.New(publicerr.InvalidTransition, reason, remedy, nil)
}

func readFacts(request ReadRequest) error {
	if request.Purged {
		return notStored(purgedReason)
	}
	selector := request.Selector
	if request.PlannedSteps != nil && (selector.StepSet || selector.Step != "") && !slices.Contains(request.PlannedSteps, selector.Step) {
		return publicerr.New(publicerr.NotFound, unknownStepReason, selectionRemedy, nil)
	}
	return nil
}
func missingSelection(saved index, request ReadRequest) error {
	selector := request.Selector
	if selector.StepSet || selector.Step != "" {
		if !slices.Contains(request.PlannedSteps, selector.Step) {
			return publicerr.New(publicerr.NotFound, unknownStepReason, selectionRemedy, nil)
		}
		for _, entry := range saved.Containers {
			if entry.Step == selector.Step {
				return publicerr.New(publicerr.NotFound, noSelectionReason, selectionRemedy, nil)
			}
		}
		return notStored(waitingReason)
	}
	return publicerr.New(publicerr.NotFound, noSelectionReason, selectionRemedy, nil)
}

func unfiltered(request ReadRequest) bool {
	return request.Selector == (Selector{}) && !request.OwnOnly
}

func matchesRequest(entry ContainerLog, request ReadRequest) bool {
	if request.OwnOnly && !entry.Main {
		return false
	}
	return matches(entry, request.Selector)
}

func (r *selected) observe(entry ContainerLog) {
	switch entry.State {
	case Following:
		r.follow = true
	case Copying:
		r.copying = true
	}
}
