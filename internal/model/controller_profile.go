package model

import "time"

// ControllerProfileStatus reports controller identity verification without exposing API
// error bodies. ReadFailuresTotal is process-local and cumulative.
type ControllerProfileStatus struct {
	DispatchBlocked   bool      `json:"dispatch_blocked"`
	Remedy            string    `json:"remedy,omitempty"`
	State             string    `json:"state"`
	Object            string    `json:"object,omitempty"`
	Reason            string    `json:"reason,omitempty"`
	ErrorClass        string    `json:"error_class,omitempty"`
	ReadFailuresTotal uint64    `json:"read_failures_total"`
	RetryAt           time.Time `json:"retry_at,omitzero"`
}
