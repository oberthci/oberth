package vmrunner

import (
	"context"

	"github.com/oberthci/oberth/internal/protectedtests"
)

// Compatibility names preserve existing journal, harness and controller APIs.
// Validation is shared with the protected generic execution preparation.
type KVMRecipe = protectedtests.Recipe
type KVMIntent = protectedtests.Intent
type KVMReport = protectedtests.Report
type KVMTerminationRecord = protectedtests.TerminationRecord
type KVMReceipt = protectedtests.Receipt
type KVMRefusal = protectedtests.Refusal
type KVMExecution = protectedtests.Execution

func KVMIntentDigest(intent KVMIntent) string { return protectedtests.IntentDigest(intent) }

// KVMJournal shares vm_executions and capacity with existing VM execution.
// An uncertain create stays submitted and keeps capacity until exact UID cleanup.
type KVMJournal interface {
	ReserveKVMPreflight(context.Context, KVMIntent) (KVMExecution, error)
	SubmitKVMPreflight(context.Context, string) error
	BindKVMPreflight(context.Context, string, string) error
	RejectKVMPreflight(context.Context, string) error
	RecordKVMReceipt(context.Context, string, KVMReceipt) error
	RecordKVMRefusal(context.Context, string, KVMRefusal) error
	CompleteKVMPreflightCleanup(context.Context, string, string) error
	KVMPreflight(context.Context, string) (KVMExecution, error)
	PendingKVMPreflights(context.Context) ([]KVMExecution, error)
}
