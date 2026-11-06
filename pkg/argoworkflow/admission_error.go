package argoworkflow

// AdmissionError is a deterministic document/configuration refusal. Retrying
// the same immutable input cannot change the result; preserve the cause for UI.
type AdmissionError struct{ Cause error }

func (e *AdmissionError) Error() string             { return e.Cause.Error() }
func (e *AdmissionError) Unwrap() error             { return e.Cause }
func (*AdmissionError) InfrastructureFailure() bool { return false }
