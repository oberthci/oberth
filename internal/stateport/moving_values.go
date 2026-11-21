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
