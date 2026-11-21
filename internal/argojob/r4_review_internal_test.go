package argojob

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestReviewR3PendingTerminalDeliveryDoesNotNeedAnotherNativeRead(t *testing.T) {
	t.Parallel()
	r := &reviewR2Reporter{refuse: 1}
	e, tr, native, _ := reviewR2Executor(t, r)
	e.process(t.Context(), tr.binding.Workflow.Name)
	if tr.final == nil || tr.completed || r.calls != 1 {
		t.Fatal("fixture did not freeze an unacknowledged terminal")
	}
	native.outage = true
	for range 20 {
		e.process(t.Context(), tr.binding.Workflow.Name)
	}
	if !tr.completed || r.calls != 2 {
		t.Fatalf("healthy reporter never receives frozen result during native outage: reports=%d completed=%v native_reads=%d tracked=%d", r.calls, tr.completed, native.reads, len(e.tracked))
	}
}

type reviewR3RecoveryRows struct {
	recoveryRunsPort
	rows    []stateport.Run
	failing bool
}

func (r *reviewR3RecoveryRows) Runs(_ context.Context, q stateport.RunFilter) ([]stateport.Run, error) {
	if q.Status == stepmodel.RunQueued && q.Cursor == "" {
		return r.rows, nil
	}
	return nil, nil
}
func (r *reviewR3RecoveryRows) Run(_ context.Context, id string) (stateport.Run, error) {
	for _, row := range r.rows {
		if row.ID == id {
			return row, nil
		}
	}
	panic("unknown row")
}
func (r *reviewR3RecoveryRows) Steps(context.Context, string) ([]stepmodel.RunStep, error) {
	if r.failing {
		return nil, errors.New("temporary recovery read refusal")
	}
	return nil, nil
}
func TestReviewR3RecoveryDiagnosticsReleasedAfterRecovery(t *testing.T) {
	t.Parallel()
	e, tr, _, _ := reviewR2Executor(t, &reviewR2Reporter{})
	e.Untrack(tr.binding.Workflow.Name)
	e.config.Progress = nil
	rows := &reviewR3RecoveryRows{failing: true}
	for i := range 200 {
		var row stateport.Run
		row.ID = fmt.Sprintf("recovered-%03d", i)
		row.Admission.RunID = row.ID
		row.Origin.WorkflowName, row.Origin.WorkflowUID = row.ID, row.ID+"-uid"
		rows.rows = append(rows.rows, row)
	}
	e.config.Runs = rows
	if err := e.recover(t.Context()); err == nil {
		t.Fatal("incomplete recovery was not reported")
	}
	if len(e.recoveryErrors) != 200 {
		t.Fatal("fixture missed recovery failures")
	}
	rows.failing = false
	if err := e.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows.rows {
		e.Untrack(row.Origin.WorkflowName)
	}
	if len(e.recoveryErrors) != 0 {
		t.Fatalf("resolved, untracked runs retain recovery diagnostics: tracked=%d errors=%d", len(e.tracked), len(e.recoveryErrors))
	}
}
