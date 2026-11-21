package stateport

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// HoldAction is an audited intention tied to the selected native attempt.
// It is not a hold state or a receipt that a command started or a release finished.
type HoldAction struct {
	RunID, Step, Kind, NodeID, PodUID string
	Revision                          int64
	Argv                              []string
	TimeoutS                          int
}

func (q HoldAction) Valid() bool {
	if q.RunID == "" || q.NodeID == "" || q.PodUID == "" {
		return false
	}
	switch q.Kind {
	case "exec":
		return len(q.Argv) > 0 && len(q.Argv) <= 256 && q.Argv[0] != "" && q.TimeoutS > 0 && q.TimeoutS <= 900
	case "abandon":
		return len(q.Argv) == 0 && q.TimeoutS == 0
	default:
		return false
	}
}

// HoldAudit identifies an inspection without retaining command arguments.
type HoldAudit struct {
	Kind, Step, Reason, NodeID, PodUID string
	Argc                               int
	ArgvSHA256                         string
}

func (q HoldAction) Audit(reason string) HoldAudit {
	raw, _ := json.Marshal(q.Argv)
	return HoldAudit{Kind: q.Kind, Step: q.Step, Reason: reason, NodeID: q.NodeID, PodUID: q.PodUID, Argc: len(q.Argv), ArgvSHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}
}
