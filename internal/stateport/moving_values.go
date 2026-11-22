package stateport

import "github.com/oberthci/oberth/internal/model"

// PublicationAttempt compares the durable attempt count before external mutation.
type PublicationAttempt struct {
	ID               string
	ExpectedAttempts int
}

// PublicationResult commits delivery and any release receipt with its events.
type PublicationResult struct {
	ID                   string
	Attempt              int
	Delivered, Retryable bool
	Reason, Detail       string
	Release              *model.ReleaseReceipt
	// ProjectCI, on a delivery, makes the publication's passing run the CI
	// result of the published branch: a fast-forward promotion leaves that
	// branch exactly at the commit the run tested. It is ordered right after
	// the runs that existed when the first push attempt started; those no
	// longer move that branch's CI issue, and any later run does.
	ProjectCI bool
}

// RecordFilter scopes records before their exclusive cursor and page limit.
// After is ascending; Before is descending. They are mutually exclusive.
type RecordFilter struct {
	Repositories                     []string
	After, Before, Status, SourceSHA string
	Limit                            int
}

// PromotionUpdate changes the result and optionally publishes existing evidence.
// Nested in RunUpdate, its publication is supplied by RunUpdate.Publication.
type PromotionUpdate struct {
	ID             string
	Revision       int64
	RunID          string
	Status         model.PromotionStatus
	Reason, Detail string
	Publication    *PublicationIntent
}
