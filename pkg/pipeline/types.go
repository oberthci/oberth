// Package pipeline defines the format-neutral pipeline vocabulary shared by
// the Argo Workflow execution engine, the store/model/service layers, and the
// CLI admission checks.
package pipeline

// MaxSourceBytes is the largest accepted pipeline definition file.
const MaxSourceBytes = 1 << 20

// Trigger determines which trust-domain pipeline executes for a run.
type Trigger string

const (
	TriggerCI      Trigger = "ci"
	TriggerRelease Trigger = "release"
)

// Valid reports whether the trigger names one isolated trust domain.
func (trigger Trigger) Valid() bool {
	switch trigger {
	case TriggerCI, TriggerRelease:
		return true
	default:
		return false
	}
}
