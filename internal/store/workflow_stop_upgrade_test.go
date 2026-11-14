package store

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestWorkflowStopUpgradePreservesOutstandingCustody(t *testing.T) {
	s := newMigrationIDTestStore(t)
	cut := -1
	for i, m := range migrations {
		if strings.Contains(m.id, "native-workflow-stop") {
			cut = i
			break
		}
	}
	if cut < 0 {
		t.Fatal("missing Stop migration")
	}
	if err := s.migrateWithMigrations(t.Context(), nil, migrations[:cut]); err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "stop", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE runs SET status='interrupted',phase='interrupted',job_name='workflow',finished_at=1 WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO run_cancellations(run_id,job_name,reason,created_at) VALUES(?,'workflow','superseded',1)`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.migrateWithMigrations(t.Context(), nil, migrations); err != nil {
		t.Fatal(err)
	}
	current, err := s.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunRunning || current.FinishedAt != nil || !current.Cancelled() {
		t.Fatalf("lost outstanding Stop: %+v %v", current, err)
	}
	pending, err := s.PendingRunCancellations(t.Context())
	if err != nil || len(pending) != 1 || pending[0].Reason != "cancelled" || pending[0].WorkflowName != "workflow" {
		t.Fatalf("lost custody: %+v %v", pending, err)
	}
}

func TestWorkflowStopUpgradeDoesNotResurrectTerminalPromotion(t *testing.T) {
	s := newMigrationIDTestStore(t)
	cut := -1
	for i, m := range migrations {
		if strings.Contains(m.id, "native-workflow-stop") {
			cut = i
			break
		}
	}
	if cut < 0 {
		t.Fatal("missing native shutdown migration")
	}
	if err := s.migrateWithMigrations(t.Context(), nil, migrations[:cut]); err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	previous, result := strings.Repeat("a", 40), strings.Repeat("b", 40)
	run, promotion, err := s.EnqueuePromotionRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/candidate", SHA: result, Actor: "owner", Trigger: "promotion", TestedSHA: result, BaseSHA: previous}, model.PromotionSpec{RepoID: repo.ID, SourceBranch: "feature/source", SourceSHA: result, TargetRef: repo.DefaultBranch, PreviousSHA: previous, ResultSHA: result, Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishPromotion(t.Context(), promotion.ID, model.PromotionFailed, run.ID, "cancelled before upgrade"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE runs SET status='interrupted',phase='cancelled',job_name='workflow',finished_at=1 WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO run_cancellations(run_id,job_name,reason,created_at) VALUES(?,'workflow','superseded',1)`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.migrateWithMigrations(t.Context(), nil, migrations); err != nil {
		t.Fatal(err)
	}
	current, err := s.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunInterrupted || current.FinishedAt == nil {
		t.Fatalf("terminal promotion resurrected: %+v %v", current, err)
	}
	pending, err := s.PendingRunCancellations(t.Context())
	if err != nil || len(pending) != 1 || !pending[0].Terminate {
		t.Fatalf("lost immediate orphan shutdown: %+v %v", pending, err)
	}
}
