package stateport

import (
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// ProgressEvent identifies a committed projection by its monotonic event sequence.
// It observes the run and never changes the revision that fences control actions.
func ProgressEvent(r Run, sequence int64, at time.Time) EventWrite {
	reason := r.DisplayReason
	if reason == "" {
		reason = string(r.Status)
	}
	return EventWrite{Dedupe: fmt.Sprintf("run.updated:%s:%d", r.ID, sequence), Event: model.AgentEvent{
		Kind: string(model.EventRunUpdated), At: at, Repo: r.Repo, RunID: r.ID, Ref: r.Ref,
		ObjectSHA: r.ObjectSHA, CommitSHA: r.CommitSHA, Summary: reason,
	}}
}
