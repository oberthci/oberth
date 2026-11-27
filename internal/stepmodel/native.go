package stepmodel

import (
	"strings"
	"time"
)

// NativeDiagnostic carries typed observations, never authority to disclose the
// native message. The service rebuilds public detail from this finite catalogue.
type NativeDiagnostic struct {
	Cause        string     `json:"cause,omitempty"`
	Source       string     `json:"source"`
	Constraints  []string   `json:"constraints,omitempty"`
	WorkflowUID  string     `json:"workflow_uid,omitempty"`
	NativeUID    string     `json:"native_uid,omitempty"`
	TransitionAt *time.Time `json:"transition_at,omitempty"`
}

func (d *NativeDiagnostic) Clone() *NativeDiagnostic {
	if d == nil {
		return nil
	}
	copy := *d
	copy.Constraints = append([]string(nil), d.Constraints...)
	copy.TransitionAt = clonePointer(d.TransitionAt)
	return &copy
}

// NativeWait distinguishes a concrete current wait from a redundant status.
func NativeWait(reason string) bool {
	switch reason {
	case "waiting for a run slot", "waiting for a lock", "waiting for a node", "waiting for quota", "image pull failed", "starting containers", "held for debugging":
		return true
	}
	return strings.HasPrefix(reason, "waiting for sync limit ") || strings.HasPrefix(reason, "waiting for mutex ")
}
