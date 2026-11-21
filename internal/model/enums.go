package model

const (
	TriggerBranch    RunTrigger = "branch"
	TriggerPromotion RunTrigger = "promotion"
	TriggerSchedule  RunTrigger = "schedule"
	TriggerTag       RunTrigger = "tag"
)

func (v RunTrigger) Valid() bool {
	return v == TriggerBranch || v == TriggerPromotion || v == TriggerSchedule || v == TriggerTag
}

// PublicationStatus describes delivery of a separate durable object.
type PublicationStatus string

const (
	PublicationPending   PublicationStatus = "pending"
	PublicationDelivered PublicationStatus = "delivered"
	PublicationFailed    PublicationStatus = "failed"
	PublicationAbandoned PublicationStatus = "abandoned"
)

func (v PublicationStatus) Valid() bool {
	return v == PublicationPending || v == PublicationDelivered || v == PublicationFailed || v == PublicationAbandoned
}

// PromotionStatus describes preparation/testing of one exact source.
type PromotionStatus string

const (
	PromotionPending     PromotionStatus = "pending"
	PromotionPassed      PromotionStatus = "passed"
	PromotionFailed      PromotionStatus = "failed"
	PromotionInterrupted PromotionStatus = "interrupted"
)

func (v PromotionStatus) Valid() bool {
	return v == PromotionPending || v == PromotionPassed || v == PromotionFailed || v == PromotionInterrupted
}

// EventKind is the complete durable observation vocabulary.
type EventKind string

const (
	EventIssueUpdated      EventKind = "issue.updated"
	EventRunFinished       EventKind = "run.finished"
	EventRunHeld           EventKind = "run.held"
	EventRunLockWait       EventKind = "run.lock_wait"
	EventPromotionFinished EventKind = "promotion.finished"
	EventReleasePublished  EventKind = "release.published"
	EventPublicationFailed EventKind = "publication.failed"
	EventRunUpdated        EventKind = "run.updated"
)

func (v EventKind) Valid() bool {
	switch v {
	case EventIssueUpdated, EventRunFinished, EventRunHeld, EventRunLockWait, EventPromotionFinished, EventReleasePublished, EventPublicationFailed, EventRunUpdated:
		return true
	}
	return false
}

// Global permits only the observation vocabulary of installation-wide facts.
func (v EventKind) Global() bool { return v == EventIssueUpdated }
