package store

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stateport"
)

// Selection by commit or ref never picks a resumed run; its own ID does, and
// a plain history read still lists it.
func TestResumedRunNeverSelectedByCommitOrRef(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	first, input := accepted(t, s, "run-one")
	input.Request.ReceiveID = "receive-run-two"
	view := input.Prepared.View()
	view.RunID, view.Workflow = "run-two", &wfv1.Workflow{}
	prepared, err := admissioncontract.Seal(view)
	requireOK(t, err)
	input.Prepared = prepared
	second, err := acceptForTest(t.Context(), s, input)
	requireOK(t, err)
	scope := []string{first.Repo}
	selected := func() (string, string) {
		t.Helper()
		byCommit, err := s.RunByCommit(t.Context(), scope, (*first.CommitSHA)[:12])
		requireOK(t, err)
		byRef, err := s.RunByRef(t.Context(), scope, first.Ref)
		requireOK(t, err)
		return byCommit.ID, byRef.ID
	}
	if commit, ref := selected(); commit != second.ID || ref != second.ID {
		t.Fatal("control: newest ordinary run not selected", commit, ref)
	}
	_, err = s.writer.ExecContext(t.Context(), `UPDATE runs SET origin=json_set(origin,'$.resumed_from',?) WHERE id=?`, first.ID, second.ID)
	requireOK(t, err)
	if commit, ref := selected(); commit != first.ID || ref != first.ID {
		t.Fatal("resumed run selected by commit or ref", commit, ref)
	}
	byID, err := s.ResolveRun(t.Context(), scope, second.ID)
	requireOK(t, err)
	rows, err := s.Runs(t.Context(), stateport.RunFilter{Repositories: scope, Limit: 10})
	requireOK(t, err)
	if !byID.Origin.Resumed() || byID.Origin.ResumedFrom != first.ID || len(rows) != 2 || !rows[0].Origin.Resumed() {
		t.Fatal("resumed run not readable by ID or history", byID.Origin, rows)
	}
	marks, err := s.RecentRuns(t.Context(), scope)
	requireOK(t, err)
	if strip := marks[first.Repo]; len(strip) != 2 || strip[0].Origin.Resumed() || strip[1].Origin.ResumedFrom != first.ID {
		t.Fatal("recent runs lost the origin", strip)
	}
}
