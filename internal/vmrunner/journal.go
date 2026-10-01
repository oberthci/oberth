package vmrunner

import (
	"context"
	"time"
)

// ExecutionIntent is written before any external create. SpecIdentity binds
// the existing admission contract; ProfileIdentity additionally binds the
// independently selected operator recipe. Neither is candidate authority.
type ExecutionIntent struct {
	Spec            VMRunSpec
	Namespace       string
	Name            string
	ProfileIdentity string
}

// Execution is durable lifecycle state only. Cleaned does not mean a passing
// suite, and this record cannot substitute for a trusted process receipt.
type Execution struct {
	Intent    ExecutionIntent
	Instance  VMInstance
	Submitted bool
	Rejected  bool
	Cleaning  bool
	Cleaned   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ExecutionJournal retains a global reservation from intent until observed
// cleanup. A submitted create without a bound UID remains an obligation after
// timeout or restart, even when a later name lookup returns NotFound.
type ExecutionJournal interface {
	ReserveVMExecution(context.Context, ExecutionIntent) (Execution, error)
	VMExecution(context.Context, string) (Execution, error)
	SubmitVMExecution(context.Context, string) error
	BindVMExecution(context.Context, string, VMInstance) error
	RejectVMExecution(context.Context, string) error
	BeginVMCleanup(context.Context, string) error
	CompleteVMCleanup(context.Context, string, VMInstance) error
	PendingVMExecutions(context.Context) ([]Execution, error)
}
