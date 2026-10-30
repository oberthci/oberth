package model

import "time"

// ControllerProfileStatus reports admission verification without exposing API
// error bodies. ReadFailuresTotal is process-local and cumulative.
type ControllerProfileStatus struct {
	AdmissionPaused   bool      `json:"admission_paused"`
	Remedy            string    `json:"remedy,omitempty"`
	State             string    `json:"state"`
	Object            string    `json:"object,omitempty"`
	Reason            string    `json:"reason,omitempty"`
	ErrorClass        string    `json:"error_class,omitempty"`
	ReadFailuresTotal uint64    `json:"read_failures_total"`
	RetryAt           time.Time `json:"retry_at,omitzero"`
}
