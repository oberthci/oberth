package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testdb"
)

const stopRestartMigrationID = "20261113153226-stop-restart-resubmission-c9820cdaf74239b4216c84bafb350153"

func TestStopRestartMigrationPreservesWorkflowsAndUncertainCreates(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	claim := func(ref string) model.Run {
		t.Helper()
		q, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, ref, strings.Repeat("a", 40)))
		if err != nil {
			t.Fatal(err)
		}
		r, err := s.ClaimNextRun(ctx)
		if err != nil || r.ID != q.ID {
			t.Fatal(r, err)
		}
		return r
	}
	old := claim("feature/reset-with-workflow")
	missing := claim("feature/reset-no-workflow")
	uncertain := claim("feature/uncertain")
	if _, err := s.SetRunWorkflowName(ctx, uncertain.ID, "uncertain-workflow"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRunCreatePending(ctx, uncertain.ID, "uncertain-workflow"); err != nil {
		t.Fatal(err)
	}
	cancelled := claim("feature/cancelled")
	if _, err := s.SetRunWorkflowName(ctx, cancelled.ID, "cancelled-workflow"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: cancelled.ID, Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/fresh", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []model.Run{old, missing} {
		if _, err := s.db.Exec(`UPDATE runs SET status='queued',phase='queued',reason='re-queued after restart' WHERE id=?`, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO run_cancellations(run_id,job_name,reason,created_at)VALUES(?,?,'owner_restart',?)`, old.ID, "original-workflow", unixNano(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(testdb.PostWorkflowStopDowngrade+`-- Reconstruct the old column because this fixture rewinds past its drop migration.
ALTER TABLE runs ADD COLUMN credentialed INTEGER NOT NULL DEFAULT 0 CHECK (credentialed IN (0, 1));
DELETE FROM schema_migrations WHERE version >= (SELECT version FROM schema_migrations WHERE id=?)`, stopRestartMigrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(historicalRestartTimingSQL(t)); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx, nil); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.Run(ctx, old.ID)
	if err != nil || recovered.Status != model.RunRunning || recovered.WorkflowName != "original-workflow" || recovered.QueueSequence != old.QueueSequence {
		t.Fatal(recovered, err)
	}
	if err := s.RecoverOwnerState(ctx); err != nil {
		t.Fatal(err)
	}
	absent, err := s.Run(ctx, missing.ID)
	if err != nil || absent.Status != model.RunFailed || absent.Error != model.ServerRestartReason {
		t.Fatal(absent, err)
	}
	preserved, err := s.Run(ctx, uncertain.ID)
	if err != nil || preserved.Status != model.RunRunning || preserved.Phase != "infrastructure-pending" {
		t.Fatal(preserved, err)
	}
	pending, err := s.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 2 {
		t.Fatal(pending, err)
	}
	for _, c := range pending {
		if c.RunID != uncertain.ID && c.RunID != cancelled.ID {
			t.Fatal("restart deletion survived migration", c)
		}
	}
	next, err := s.ClaimNextRun(ctx)
	if err != nil || next.ID != fresh.ID {
		t.Fatal("migration re-submitted old work", next, err)
	}
	if count, err := s.CountRuns(ctx); err != nil || count.Total != 5 {
		t.Fatal(count, err)
	}
}

func applyStopRestartMigration(t *testing.T, s *Store) {
	t.Helper()
	for _, migration := range migrations {
		if migration.id == stopRestartMigrationID {
			const retiredColumns = `ALTER TABLE runs ADD COLUMN superseded_by TEXT NOT NULL DEFAULT ''; ALTER TABLE run_cancellations ADD COLUMN superseded_by TEXT NOT NULL DEFAULT '';`
			const removeRetiredColumns = `ALTER TABLE runs DROP COLUMN superseded_by; ALTER TABLE run_cancellations DROP COLUMN superseded_by;`
			if _, err := s.db.Exec(retiredColumns + migration.sql + removeRetiredColumns); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("missing stop-restart migration")
}
