package stepmodel

import (
	"slices"
	"time"
)

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (h RetryHint) Clone() RetryHint {
	h.After = clonePointer[time.Time](h.After)
	return h
}

func (f FailureRecord) Clone() FailureRecord {
	f.Retry = f.Retry.Clone()
	return f
}

func (s RunStep) Clone() RunStep {
	s.Diagnostic = s.Diagnostic.Clone()
	s.StartedAt = clonePointer(s.StartedAt)
	s.FinishedAt = clonePointer(s.FinishedAt)
	s.Attempts = slices.Clone(s.Attempts)
	s.Logs = slices.Clone(s.Logs)
	return s
}

func (r RunSummary) Clone() RunSummary {
	r.Diagnostic = r.Diagnostic.Clone()
	r.CommitSHA = clonePointer(r.CommitSHA)
	r.NativePhase = clonePointer(r.NativePhase)
	r.StartedAt = clonePointer(r.StartedAt)
	r.FinishedAt = clonePointer(r.FinishedAt)
	r.NextStepOffset = clonePointer(r.NextStepOffset)
	r.Steps = slices.Clone(r.Steps)
	for i := range r.Steps {
		r.Steps[i] = r.Steps[i].Clone()
	}
	if r.Failure != nil {
		copy := r.Failure.Clone()
		r.Failure = &copy
	}
	return r
}
