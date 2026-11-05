package model

import (
	"encoding/json"
	"time"
)

// ExecutionTargetBinding is canonical control-plane state, never a workload
// annotation or a result imported from an executor. Snapshot contains public
// defaulted configuration and retained source/fragment bytes, not credentials.
type ExecutionTargetBinding struct {
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
