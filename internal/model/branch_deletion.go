package model

import "time"

// BranchDeletionStatus is the upstream state of one accepted branch deletion.
// The local mirror ref is already gone when a record exists; the status says
// whether the forge has caught up.
type BranchDeletionStatus string

const (
	BranchDeletionPending    BranchDeletionStatus = "pending"
	BranchDeletionPropagated BranchDeletionStatus = "propagated"
	BranchDeletionFailed     BranchDeletionStatus = "failed"
	BranchDeletionSkipped    BranchDeletionStatus = "skipped"
)

func (s BranchDeletionStatus) Valid() bool {
	switch s {
	case BranchDeletionPending, BranchDeletionPropagated, BranchDeletionFailed, BranchDeletionSkipped:
		return true
	default:
		return false
	}
}

// BranchDeletionSource names the path that accepted the deletion.
const (
	BranchDeletionFromPush  = "push"
	BranchDeletionFromPrune = "prune"
)

type BranchDeletion struct {
	Sequence   int64
	ID         string
	RepoID     int64
	Repository string
	Branch     string
	OldSHA     string
	Actor      string
	Source     string
	BatchID    string
	Status     BranchDeletionStatus
	Attempts   int
	RetryAfter time.Time
	Error      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// BranchDeletionUpdate moves one pending record. Only pending rows change.
type BranchDeletionUpdate struct {
	ID         string
	Status     BranchDeletionStatus
	Attempts   int
	RetryAfter time.Time
	Error      string
}

// BranchPruneEntry is one exact branch head a confirmed prune plan deletes.
type BranchPruneEntry struct {
	Branch string
	SHA    string
}

type BranchDeletionFilter struct {
	RepoID int64
	Status BranchDeletionStatus
	Before int64
	Limit  int
}
