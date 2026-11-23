package gitport

import "github.com/oberthci/oberth/internal/publicerr"

const (
	ForgeHistoryLostReason = EmptyForgeReason
	ForgeHistoryLostRemedy = EmptyForgeRemedy
)

// ForgeHistoryLostError refuses an emptied forge once its history was tracked.
// It is distinct from EmptyRepositoryError, which permits bootstrap.
type ForgeHistoryLostError struct{}

func (*ForgeHistoryLostError) Reason() string  { return ForgeHistoryLostReason }
func (*ForgeHistoryLostError) Remedy() string  { return ForgeHistoryLostRemedy }
func (e *ForgeHistoryLostError) Error() string { return e.Reason() + " " + e.Remedy() }
func (e *ForgeHistoryLostError) Unwrap() error {
	return publicerr.New(publicerr.InvalidInput, e.Reason(), e.Remedy(), nil)
}
