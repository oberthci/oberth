package model

import (
	"encoding/json"
	"errors"
	"regexp"
	"time"
	"unicode/utf8"
)

// StepDiagnostic is a bounded Kubernetes observation, not execution authority.
// Nil persisted values predate collection and are exposed as unavailable.
type StepDiagnostic struct {
	Available   bool             `json:"available"`
	Unavailable string           `json:"unavailable,omitempty"`
	ObservedAt  *time.Time       `json:"observed_at,omitempty"`
	Container   *StepTermination `json:"container,omitempty"`
}

type StepTermination struct {
	RunID                 string    `json:"run_id"`
	Namespace             string    `json:"namespace"`
	WorkflowName          string    `json:"workflow_name"`
	WorkflowUID           string    `json:"workflow_uid"`
	NodeID                string    `json:"node_id"`
	PodName               string    `json:"pod_name"`
	PodUID                string    `json:"pod_uid"`
	ContainerKind         string    `json:"container_kind,omitempty"`
	Message               string    `json:"message,omitempty"`
	MessageTruncated      bool      `json:"message_truncated,omitempty"`
	ContainerName         string    `json:"container_name"`
	ContainerID           string    `json:"container_id"`
	RestartCount          int32     `json:"restart_count"`
	Reason                string    `json:"reason"`
	PodReason             string    `json:"pod_reason"`
	ExitCode              int32     `json:"exit_code"`
	Signal                int32     `json:"signal"`
	StartedAt             time.Time `json:"started_at"`
	FinishedAt            time.Time `json:"finished_at"`
	CPURequestMilli       *int64    `json:"cpu_request_milli,omitempty"`
	CPULimitMilli         *int64    `json:"cpu_limit_milli,omitempty"`
	MemoryRequestBytes    *int64    `json:"memory_request_bytes,omitempty"`
	MemoryLimitBytes      *int64    `json:"memory_limit_bytes,omitempty"`
	ActiveDeadlineSeconds *int64    `json:"active_deadline_seconds,omitempty"`
}

var diagnosticName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
var diagnosticUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var diagnosticRun = regexp.MustCompile(`^[0-9a-f]{32}$`)
var diagnosticCID = regexp.MustCompile(`^(containerd|docker|cri-o)://[0-9a-f]{64}$`)

func UnavailableStepDiagnostic(reason string) *StepDiagnostic {
	return &StepDiagnostic{Unavailable: reason}
}

func (d StepDiagnostic) Validate() error {
	fail := errors.New("invalid step diagnostic")
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > 4096 {
		return fail
	}
	if !d.Available {
		if d.Container != nil {
			return fail
		}
		switch d.Unavailable {
		case "not-recorded", "not-executed", "unsupported-shape", "ambiguous-attempt", "snapshot-incomplete", "observation-unavailable", "pod-absent", "binding-mismatch", "termination-unavailable", "invalid-termination":
		default:
			return fail
		}
		if d.ObservedAt != nil && d.ObservedAt.IsZero() {
			return fail
		}
		return nil
	}
	if d.Unavailable != "" || d.ObservedAt == nil || d.ObservedAt.IsZero() || d.Container == nil {
		return fail
	}
	c := d.Container
	if !diagnosticRun.MatchString(c.RunID) || !diagnosticUID.MatchString(c.WorkflowUID) || !diagnosticUID.MatchString(c.PodUID) || !diagnosticCID.MatchString(c.ContainerID) {
		return fail
	}
	if (c.ContainerKind != "" && c.ContainerKind != "main" && c.ContainerKind != "init") ||
		(c.ContainerKind != "init" && c.ContainerName != "main") ||
		!diagnosticName.MatchString(c.ContainerName) || len(c.Message) > 1536 || !utf8.ValidString(c.Message) {
		return fail
	}
	for _, s := range []string{c.Namespace, c.WorkflowName, c.NodeID, c.PodName} {
		if !diagnosticName.MatchString(s) {
			return fail
		}
	}
	if c.RestartCount < 0 || c.ExitCode < 0 || c.Signal < 0 || c.Signal > 64 || c.StartedAt.IsZero() || c.FinishedAt.Before(c.StartedAt) || c.FinishedAt.After(*d.ObservedAt) {
		return fail
	}
	switch c.Reason {
	case "Completed", "Error", "OOMKilled", "ContainerCannotRun", "other":
	default:
		return fail
	}
	if (c.Reason == "Completed" && (c.ExitCode != 0 || c.Signal != 0)) || ((c.Reason == "Error" || c.Reason == "OOMKilled") && c.ExitCode == 0) {
		return fail
	}
	switch c.PodReason {
	case "none", "DeadlineExceeded", "other":
	default:
		return fail
	}
	for _, p := range []*int64{c.CPURequestMilli, c.CPULimitMilli, c.MemoryRequestBytes, c.MemoryLimitBytes, c.ActiveDeadlineSeconds} {
		if p != nil && (*p < 0 || *p > 1<<60) {
			return fail
		}
	}
	return nil
}
