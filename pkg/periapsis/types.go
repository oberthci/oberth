// Package periapsis defines the format-neutral pipeline vocabulary shared by
// the Argo Workflow execution engine, the store/model/service layers, and the
// CLI admission checks.
package periapsis

// Size is the declared resource tier of one pipeline step.
type Size string

const (
	S  Size = "S"
	M  Size = "M"
	L  Size = "L"
	XL Size = "XL"
)

// Effective returns the declared size, or M for an undeclared step.
func (s Size) Effective() Size {
	if s == "" {
		return M
	}
	return s
}

// Valid reports whether the size is one of the public resource tiers. The
// zero value is valid because undeclared steps are M for compatibility.
func (s Size) Valid() bool {
	switch s {
	case "", S, M, L, XL:
		return true
	default:
		return false
	}
}

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
