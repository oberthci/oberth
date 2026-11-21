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

const EmptyForgeReason = "The forge repository has no commit."
const EmptyForgeRemedy = "Push a first commit to the forge, then retry."

type EmptyRepositoryError struct{}

func (*EmptyRepositoryError) Reason() string  { return EmptyForgeReason }
func (*EmptyRepositoryError) Remedy() string  { return EmptyForgeRemedy }
func (e *EmptyRepositoryError) Error() string { return e.Reason() + " " + e.Remedy() }
func (e *EmptyRepositoryError) Unwrap() error {
	return publicerr.New(publicerr.InvalidInput, e.Reason(), e.Remedy(), nil)
}
