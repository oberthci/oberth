package gitport

import "github.com/oberthci/oberth/internal/publicerr"

const (
	ForgeStatusReason   = "forge unavailable"
	ForgeReason         = "The forge could not be reached."
	ForgeRemedy         = "Retry when the forge is reachable; ask the server operator if it stays unavailable."
	ForgeOperatorRemedy = "Check the forge address, server key and host pins, then retry."
	ForgeCacheNotice    = "Forge did not answer; serving cache aged "
)

// ReceivePendingID marks a publication the cache cannot record yet because a
// receive holds the public refs; the forge may already have the commit.
const ReceivePendingID = "receive-pending"

const (
	CloneUnfinishedReason = "First clone did not finish in time."
	CloneUnfinishedRemedy = "It continues on the server, so try again in a few minutes; a clone that failed starts again then."
)

// CloneUnfinishedError means the forge answered but the first clone was
// still running when the caller's deadline passed; the clone continues.
type CloneUnfinishedError struct{ Cause error }

func (*CloneUnfinishedError) Reason() string  { return CloneUnfinishedReason }
func (*CloneUnfinishedError) Remedy() string  { return CloneUnfinishedRemedy }
func (e *CloneUnfinishedError) Error() string { return e.Reason() + " " + e.Remedy() }
func (e *CloneUnfinishedError) Unwrap() error {
	return publicerr.New(publicerr.Unavailable, e.Reason(), e.Remedy(), e.Cause)
}

const (
	FetchUnfinishedReason = "The forge copy is still updating."
	FetchUnfinishedRemedy = "It continues on the server, so try again in a minute."
	// ForgeUpdatingNotice heads the line a read served from the cache prints
	// while the repository's forge fetch still runs.
	ForgeUpdatingNotice = "Forge copy is still updating; serving cache aged "
)

// FetchUnfinishedError means the forge answered but the repository's fetch
// was still running when the caller's wait ended; the fetch continues.
type FetchUnfinishedError struct{ Cause error }

func (*FetchUnfinishedError) Reason() string  { return FetchUnfinishedReason }
func (*FetchUnfinishedError) Remedy() string  { return FetchUnfinishedRemedy }
func (e *FetchUnfinishedError) Error() string { return e.Reason() + " " + e.Remedy() }
func (e *FetchUnfinishedError) Unwrap() error {
	return publicerr.New(publicerr.Unavailable, e.Reason(), e.Remedy(), e.Cause)
}

const EmptyForgeReason = "The forge repository has no commit."

// EmptyForgeRemedy names the bootstrap path when an operation needs a commit.
const EmptyForgeRemedy = "As administrator, push the first commit to the default branch through the oberth remote."

type EmptyRepositoryError struct{}

func (*EmptyRepositoryError) Reason() string  { return EmptyForgeReason }
func (*EmptyRepositoryError) Remedy() string  { return EmptyForgeRemedy }
func (e *EmptyRepositoryError) Error() string { return e.Reason() + " " + e.Remedy() }
func (e *EmptyRepositoryError) Unwrap() error {
	return publicerr.New(publicerr.InvalidInput, e.Reason(), e.Remedy(), nil)
}
