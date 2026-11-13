package model

import (
	"encoding/json"
	"time"
)

// ExecutionTargetBinding is canonical control-plane state, never a workload
// annotation or a result imported from an executor. Snapshot contains public
// defaulted configuration and retained source/fragment bytes, not credentials.
type ExecutionTargetBinding struct {
	// CustodyDigest retains the original object-binding identity across a configuration migration.
	CustodyDigest                                                                          string `json:"-"`
	Trigger, ScheduleEntry                                                                 string
	RefKind                                                                                string
	RunID, Workflow, Target, Repository, Ref, SHA, TestedSHA                               string
	RepositoryID                                                                           int64
	Namespace, PolicyBaseline, PolicyDigest, SourceDigest, GraphDigest, FragmentLockDigest string
	Snapshot                                                                               json.RawMessage
}

type ExecutionTargetResource struct {
	RunID, Kind, Namespace, Name, UID string
	Object                            json.RawMessage
	NotBefore                         time.Time
}

// ExecutionTargetRecoveryStatus exposes only a count and fixed operator remedy,
// never retained repository identities, target configuration or object bodies.
type ExecutionTargetRecoveryStatus struct {
	QuarantinedBindings int64  `json:"quarantined_bindings"`
	Reason              string `json:"reason"`
	Remedy              string `json:"remedy"`
}

const ExecutionTargetRecoveryReason = "Retained execution target bindings have inconsistent original resource custody."
const ExecutionTargetRecoveryRemedy = "Inspect retained bindings and original resource intents against a verified pre-upgrade backup; reconcile the original objects manually and start a fresh run. Quarantined bindings cannot be reused."
