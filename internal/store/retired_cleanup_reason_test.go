package store

import "testing"

func TestRetiredSpecMismatchIsNotAWorkflowCleanupReason(t *testing.T) {
	if WorkflowCleanupReason("Workflow spec mismatch; cleanup pending") {
		t.Fatal("retired spec comparison still authorizes cleanup")
	}
}
