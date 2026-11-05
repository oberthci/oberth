package model

import (
	"errors"
	"strings"
)

// ResourceAttemptInventory records the Workflow's observed attempt set before
// Pod discovery. Complete describes node visibility, not metric sufficiency.
// Neither a missing Pod UID nor a missing metric is replaced by a fabricated ID.
type ResourceAttemptInventory struct {
	RunID        string            `json:"run_id"`
	WorkflowName string            `json:"workflow_name"`
	WorkflowUID  string            `json:"workflow_uid"`
	Namespace    string            `json:"namespace"`
	Complete     bool              `json:"complete"`
	Terminal     bool              `json:"terminal"`
	Attempts     []ResourceAttempt `json:"-"`
}

type ResourceAttempt struct {
	NodeID   string     `json:"node_id"`
	Stage    string     `json:"burn"`
	Step     string     `json:"step"`
	Template string     `json:"template"`
	Owner    string     `json:"owner_repository,omitempty"`
	Status   StepStatus `json:"status"`
	PodUID   string     `json:"pod_uid,omitempty"`
}

// ResourceAttemptGap identifies missing/failed coverage without inventing a
// Pod, runtime incarnation, metric, termination time or physical failure cause.
type ResourceAttemptGap struct {
	RunID, WorkflowUID, NodeID, Stage, Template, Owner string
}

func (i ResourceAttemptInventory) Validate() error {
	bad := errors.New("invalid resource attempt inventory")
	if !diagnosticRun.MatchString(i.RunID) || !diagnosticUID.MatchString(i.WorkflowUID) || !diagnosticName.MatchString(i.WorkflowName) || !diagnosticName.MatchString(i.Namespace) || len(i.Attempts) > 2048 {
		return bad
	}
	seen := map[string]bool{}
	for _, a := range i.Attempts {
		for _, name := range []string{a.NodeID, a.Stage, a.Step, a.Template} {
			if !diagnosticName.MatchString(name) {
				return bad
			}
		}
		if seen[a.NodeID] || !a.Status.Valid() || a.Status == StepSkipped || (a.PodUID != "" && !diagnosticUID.MatchString(a.PodUID)) || len(a.Owner) > 253 || strings.ContainsAny(a.Owner, "\x00\r\n\t ") {
			return bad
		}
		seen[a.NodeID] = true
	}
	return nil
}
