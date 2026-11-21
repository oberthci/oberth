package stepmodel

const (
	StepPending StepStatus = "pending"
	StepQueued  StepStatus = "queued"
	StepRunning StepStatus = "running"
	StepPassed  StepStatus = "passed"
	StepFailed  StepStatus = "failed"
	StepSkipped StepStatus = "skipped"
	StepUnknown StepStatus = "unknown"
)

func (s StepStatus) Valid() bool {
	switch s {
	case StepPending, StepQueued, StepRunning, StepPassed, StepFailed, StepSkipped, StepUnknown:
		return true
	}
	return false
}

const (
	FailureUnknown        FailureClass = "unknown"
	FailureAuthor         FailureClass = "author"
	FailureInfrastructure FailureClass = "infrastructure"
	FailureDeadline       FailureClass = "deadline"
	FailureCancelled      FailureClass = "cancelled"
	FailureInvariant      FailureClass = "invariant"
)

func (c FailureClass) Valid() bool {
	switch c {
	case FailureUnknown, FailureAuthor, FailureInfrastructure, FailureDeadline, FailureCancelled, FailureInvariant:
		return true
	}
	return false
}

// RetryAction is advice; it never starts or schedules another attempt.
type RetryAction string

const (
	RetryNone        RetryAction = "none"
	RetryRerun       RetryAction = "rerun"
	RetryPublication RetryAction = "retry_publication"
	RetryWait        RetryAction = "wait"
)

func (a RetryAction) Valid() bool {
	switch a {
	case RetryNone, RetryRerun, RetryPublication, RetryWait:
		return true
	}
	return false
}

// RunStatus is the sole run verdict, independent of publication and native evidence.
type RunStatus string

const (
	RunQueued    RunStatus = "queued"
	RunRunning   RunStatus = "running"
	RunPassed    RunStatus = "passed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

func (s RunStatus) Valid() bool {
	switch s {
	case RunQueued, RunRunning, RunPassed, RunFailed, RunCancelled:
		return true
	}
	return false
}

// NativePhase retains observed native evidence without changing the run verdict.
type NativePhase string

const (
	NativePending   NativePhase = "Pending"
	NativeRunning   NativePhase = "Running"
	NativeSucceeded NativePhase = "Succeeded"
	NativeFailed    NativePhase = "Failed"
	NativeError     NativePhase = "Error"
	NativeUnknown   NativePhase = "Unknown"
)

func (p NativePhase) Valid() bool {
	switch p {
	case NativePending, NativeRunning, NativeSucceeded, NativeFailed, NativeError, NativeUnknown:
		return true
	}
	return false
}
