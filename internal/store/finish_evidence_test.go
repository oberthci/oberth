package store

import (
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

func TestArmedFinishMarkCommitsWithVerdict(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	// The fixture seal stands for the native transform's arming fact.
	_, input := accepted(t, s, "armed-atomic")
	view := input.Prepared.View()
	view.RunID = "armed-finish"
	view.Plan.Entries[0].HoldArmed = true
	input.Prepared = sealView(t, view)
	input.Request.ReceiveID = "armed-finish"
	r, err := acceptForTest(t.Context(), s, input)
	requireOK(t, err)
	update := stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed, NativePhase: new(stepmodel.NativeSucceeded)}
	requireOK(t, createFinishCommitFailure(t, s))
	if err = s.Finish(t.Context(), stateport.Action{}, update); err == nil {
		t.Fatal("commit failure accepted finish")
	}
	unchanged, err := s.Run(t.Context(), r.ID)
	requireOK(t, err)
	if unchanged.Origin.Assisted || unchanged.Status != r.Status || unchanged.Revision != r.Revision {
		t.Fatal("failed finish leaked mark or verdict", unchanged)
	}
	_, err = s.writer.ExecContext(t.Context(), `DROP TRIGGER refuse_armed_finish`)
	requireOK(t, err)
	requireOK(t, s.Finish(t.Context(), stateport.Action{}, update))
	_, err = s.writer.ExecContext(t.Context(), `DROP TABLE finish_commit_failure`)
	requireOK(t, err)
	requireOK(t, s.Close())
	// A second independent store instance reads the committed bytes.
	reopened, err := Open(t.Context(), s.path, testOptions())
	requireOK(t, err)
	defer reopened.Close()
	stored, err := reopened.Run(t.Context(), r.ID)
	requireOK(t, err)
	if !stored.Origin.Assisted || stored.Status != stepmodel.RunPassed {
		t.Fatal("reopen lost atomic evidence decision", stored)
	}
}
func createFinishCommitFailure(t *testing.T, s *Store) error {
	t.Helper()
	_, err := s.writer.ExecContext(t.Context(), `CREATE TABLE finish_commit_failure (run_id TEXT REFERENCES runs(id) DEFERRABLE INITIALLY DEFERRED)`)
	if err != nil {
		return err
	}
	_, err = s.writer.ExecContext(t.Context(), `CREATE TRIGGER refuse_armed_finish AFTER UPDATE OF origin ON runs WHEN json_extract(NEW.origin,'$.assisted')=1 BEGIN INSERT INTO finish_commit_failure VALUES('absent'); END`)
	return err
}
