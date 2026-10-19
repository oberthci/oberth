package model

import "time"

type MaintenanceState string

const (
	MaintenanceBooked   MaintenanceState = "booked"
	MaintenanceDraining MaintenanceState = "draining"
	MaintenanceActive   MaintenanceState = "active"
	MaintenanceEnded    MaintenanceState = "ended"
	MaintenanceAborted  MaintenanceState = "aborted"
	MaintenanceActor                     = "oberth:maintenance"
)

func (s MaintenanceState) Open() bool {
	return s == MaintenanceBooked || s == MaintenanceDraining || s == MaintenanceActive
}

func (s MaintenanceState) Holds() bool {
	return s == MaintenanceDraining || s == MaintenanceActive
}

type MaintenanceWindow struct {
	ID, BookedBy, Reason, BookedVersion, ForcedBy, EndedBy, EndReason, ResumedVersion string
	State                                                                             MaintenanceState
	NotBefore                                                                         time.Time
	DrainTimeout                                                                      time.Duration
	AutoEnd                                                                           time.Duration
	DrainStartedAt                                                                    *time.Time
	ForcedAt                                                                          *time.Time
	StartedAt                                                                         *time.Time
	ResumedAt                                                                         *time.Time
	EndedAt                                                                           *time.Time
	CreatedAt                                                                         time.Time
	UpdatedAt                                                                         time.Time
}

type MaintenanceSpec struct {
	BookedBy, Reason, BookedVersion string
	NotBefore                       time.Time
	DrainTimeout                    time.Duration
	AutoEnd                         time.Duration
}

type MaintenanceHold struct {
	WindowID  string           `json:"window_id"`
	State     MaintenanceState `json:"state"`
	BookedBy  string           `json:"booked_by"`
	Reason    string           `json:"reason,omitempty"`
	NotBefore time.Time        `json:"not_before"`
}

type MaintenanceProgress struct {
	Window               MaintenanceWindow `json:"window"`
	RunningRuns          int               `json:"running_runs"`
	ProtectedRuns        int               `json:"protected_runs"`
	HeldRuns             int               `json:"held_runs"`
	PendingCancellations int               `json:"pending_cancellations"`
	DrainDeadline        *time.Time        `json:"drain_deadline,omitempty"`
	AutoEndAt            *time.Time        `json:"auto_end_at,omitempty"`
}
