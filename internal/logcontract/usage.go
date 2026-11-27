package logcontract

import "time"

// StorageBytes separates apparent file length from filesystem allocation.
type StorageBytes struct {
	Logical   int64 `json:"logical"`
	Allocated int64 `json:"allocated"`
}

// Usage describes one physical archive, independently of a selected log window.
// Eligibility stays unknown until the ordered purge owner supplies its fence.
type Usage struct {
	PhysicalRunID      string       `json:"physical_run_id"`
	SampledAt          time.Time    `json:"sampled_at"`
	Body               StorageBytes `json:"body"`
	Index              StorageBytes `json:"index"`
	CaptureLimitBytes  int64        `json:"capture_limit_bytes"`
	CaptureLimited     bool         `json:"capture_limited"`
	CaptureReason      string       `json:"capture_reason,omitempty"`
	Incomplete         bool         `json:"incomplete"`
	AccountingComplete bool         `json:"accounting_complete"`
	PurgePending       bool         `json:"purge_pending"`
	PurgeCompleted     bool         `json:"purge_completed"`
	PurgeTime          *time.Time   `json:"purge_time"` // Legacy receipts have no completion timestamp.
	ProtectionReason   string       `json:"protection_reason"`
	EligibilityKnown   bool         `json:"eligibility_known"`
}

// StoragePreview is an advisory, non-atomic directory inventory. Unknown entries
// remain charged and make accounting incomplete. No field promises reclamation.
type StoragePreview struct {
	Atomic               bool         `json:"atomic"`
	StartedAt            time.Time    `json:"started_at"`
	FinishedAt           time.Time    `json:"finished_at"`
	Complete             bool         `json:"complete"`
	Body                 StorageBytes `json:"body"`
	Index                StorageBytes `json:"index"`
	Staging              StorageBytes `json:"staging"`
	Unknown              StorageBytes `json:"unknown"`
	Directory            StorageBytes `json:"directory"`
	OpenSpools           StorageBytes `json:"open_spools"`
	OpenSpoolCount       int          `json:"open_spool_count"`
	ActiveReservations   int          `json:"active_reservations"`
	UnknownEntries       int          `json:"unknown_entries"`
	NonregularEntries    int          `json:"nonregular_entries"`
	LogicalBytes         int64        `json:"logical_bytes"`
	AllocatedBytes       int64        `json:"allocated_bytes"`
	FreeBytes            uint64       `json:"free_bytes"`
	FreeInodes           uint64       `json:"free_inodes"`
	BudgetBytes          int64        `json:"budget_bytes"`
	ReviewBytes          int64        `json:"review_bytes"`
	TargetBytes          int64        `json:"target_bytes"`
	BudgetHeadroomBytes  *int64       `json:"budget_headroom_bytes"`
	RequiredReserveBytes *int64       `json:"required_reserve_bytes"` // Includes unobserved queued children/drain growth; unknown here.
	ReclaimableBytes     *int64       `json:"reclaimable_bytes"`      // Requires the later ordered purge protocol.
}
