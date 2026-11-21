package stepmodel

import "time"

// StepStatus is pending, queued, running, passed, failed, skipped or unknown.
type StepStatus string

type StepAttempt struct {
	NodeID  string     `json:"node_id"`
	PodName string     `json:"pod_name"`
	Status  StepStatus `json:"status"`
}

type LogicalStepCounts struct {
	Total   int  `json:"step_count"`
	Exact   bool `json:"steps_planned_exact"`
	Pending int  `json:"pending"`
	Queued  int  `json:"queued"`
	Running int  `json:"running"`
	Passed  int  `json:"passed"`
	Failed  int  `json:"failed"`
	Skipped int  `json:"skipped"`
	Unknown int  `json:"unknown"`
}

// FailureClass is unknown, author, infrastructure, deadline, cancelled or invariant.
type FailureClass string

type RetryHint struct {
	Action     RetryAction `json:"action"`
	ReasonCode string      `json:"reason_code"`
	After      *time.Time  `json:"after,omitempty"`
}

type FailureRecord struct {
	Class  FailureClass `json:"failure_class"`
	Step   string       `json:"failed_step"`
	Name   string       `json:"failed_step_name"`
	NodeID string       `json:"node_id"`
	Retry  RetryHint    `json:"retry_hint"`
}
