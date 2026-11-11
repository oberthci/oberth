package model

import (
	"strings"
	"time"
)

// NodeBootObservation binds a kubelet boot ID to one owned step Pod. It is
// private execution evidence, retained before termination and across resumes.
type NodeBootObservation struct {
	RunID, WorkflowName, WorkflowUID, NodeID, PodUID string
	NodeName, NodeUID, BootID                        string
	StartedAt, ObservedAt                            time.Time
}

func (o NodeBootObservation) Valid() bool {
	return diagnosticRun.MatchString(o.RunID) && timingName.MatchString(o.WorkflowName) && diagnosticUID.MatchString(o.WorkflowUID) && timingName.MatchString(o.NodeID) && diagnosticUID.MatchString(o.PodUID) && timingName.MatchString(o.NodeName) && diagnosticUID.MatchString(o.NodeUID) && o.BootID != "" && len(o.BootID) <= 256 && !strings.ContainsAny(o.BootID, "\x00\r\n") && !o.ObservedAt.IsZero() && (o.StartedAt.IsZero() || !o.ObservedAt.Before(o.StartedAt))
}

// NodeReboot bounds a restart between two API observations; Kubernetes exposes
// a boot ID, not the exact OS boot timestamp. Never present discovery as boot time.
type NodeReboot struct {
	NodeName          string
	After, ObservedAt time.Time
}
