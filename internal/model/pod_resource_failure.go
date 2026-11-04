package model

import (
	"errors"
	"strings"
	"time"
)

// PodResourceDisposition is server-observed disposition, independent of a runtime
// container ID or valid main termination. It carries no Pod message or logs.
type PodResourceDisposition struct {
	Step         string    `json:"step"`
	RunID        string    `json:"run_id"`
	WorkflowName string    `json:"workflow_name"`
	WorkflowUID  string    `json:"workflow_uid"`
	Namespace    string    `json:"namespace"`
	NodeID       string    `json:"node_id"`
	PodName      string    `json:"pod_name"`
	PodUID       string    `json:"pod_uid"`
	Stage        string    `json:"burn"`
	Template     string    `json:"template"`
	Owner        string    `json:"owner_repository,omitempty"`
	Reason       string    `json:"reason"`
	ObservedAt   time.Time `json:"observed_at"`
}

func (f PodResourceDisposition) Validate() error {
	bad := errors.New("invalid Pod resource failure binding")
	if !diagnosticRun.MatchString(f.RunID) || !diagnosticUID.MatchString(f.WorkflowUID) || !diagnosticUID.MatchString(f.PodUID) || f.ObservedAt.IsZero() {
		return bad
	}
	for _, s := range []string{f.WorkflowName, f.Namespace, f.NodeID, f.PodName, f.Stage, f.Step, f.Template} {
		if !diagnosticName.MatchString(s) {
			return bad
		}
	}
	if len(f.Owner) > 253 || strings.ContainsAny(f.Owner, "\x00\r\n\t ") {
		return bad
	}
	if f.Reason != "Evicted" && f.Reason != "DeadlineExceeded" && f.Reason != "Failed" && f.Reason != "Completed" {
		return bad
	}
	return nil
}
