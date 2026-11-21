package argojob

import (
	"github.com/oberthci/oberth/internal/stateport"
	"testing"
)

func TestUntrackingDropsUnresolvedRecoveryDiagnostic(t *testing.T) {
	t.Parallel()
	e := New(Config{})
	defer e.queue.ShutDown()
	e.recoveryErrors["workflow"] = "unreadable row"
	e.Untrack("workflow")
	if len(e.recoveryErrors) != 0 {
		t.Fatal("untracked recovery failure retained", e.recoveryErrors)
	}
}

func TestSuccessfulRestoreDropsDiagnosticBeforeUntrack(t *testing.T) {
	t.Parallel()
	e, tr, _, _ := reviewR2Executor(t, &reviewR2Reporter{})
	e.Untrack(tr.binding.Workflow.Name)
	e.config.Progress = nil
	var row stateport.Run
	row.ID = "recovered"
	row.Admission.RunID = row.ID
	row.Origin.WorkflowName, row.Origin.WorkflowUID = "workflow", "uid"
	e.config.Runs = &reviewR3RecoveryRows{rows: []stateport.Run{row}}
	e.recoveryErrors[row.Origin.WorkflowName] = "unreadable row"
	if err := e.restore(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if len(e.recoveryErrors) != 0 {
		t.Fatal("recovered run retains its diagnostic", e.recoveryErrors)
	}
}
