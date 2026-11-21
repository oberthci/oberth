package admissioncontract

// Mode selects preparation for submission or read-only inspection.
type Mode string

const (
	ModeSubmit  Mode = "submit"
	ModeInspect Mode = "inspect"
)

func (m Mode) Valid() bool {
	switch m {
	case ModeSubmit, ModeInspect:
		return true
	}
	return false
}

// EvidenceState distinguishes checked facts from missing live knowledge.
// Only verified evidence can support submission authority.
type EvidenceState string

const (
	StateVerified     EvidenceState = "verified"
	StateLiveRequired EvidenceState = "live_required"
	StateUnknown      EvidenceState = "unknown"
)

func (s EvidenceState) Valid() bool {
	switch s {
	case StateVerified, StateLiveRequired, StateUnknown:
		return true
	}
	return false
}
