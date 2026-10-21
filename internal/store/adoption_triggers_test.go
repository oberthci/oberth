package store

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestOwnerRecoveryRetiresLegacyTriggerRestartDeletionButKeepsCancel(t *testing.T) {
	for _, kind := range []string{"promotion", "tag", "release"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now().UTC()
			s := testStore(t, &now)
			ctx := context.Background()
			repo := createRepo(t, s)
			spec := model.RunSpec{RepoID: repo.ID, RefKind: model.RefTag, Ref: "v1.2.3", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TestedSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Actor: "agent@host", Trigger: kind, Release: true, Credentialed: true}
			var enqueued model.EnqueueRunResult
			var err error
			if kind == "promotion" {
				spec.Release = false
				spec.RefKind = model.RefBranch
				spec.Ref = "promotion/main/bbbbbbbbbbbb"
				spec.SHA = spec.TestedSHA
				spec.BaseSHA = "cccccccccccccccccccccccccccccccccccccccc"
				enqueued, _, err = s.EnqueuePromotionRun(ctx, spec, model.PromotionSpec{RepoID: repo.ID, SourceBranch: "feature/adopt", SourceSHA: spec.SHA, TargetRef: "main", PreviousSHA: spec.BaseSHA, ResultSHA: spec.SHA, Actor: spec.Actor})
			} else {
				enqueued, err = s.EnqueueRun(ctx, spec)
			}
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.ClaimNextRun(ctx)
			if err != nil || run.ID != enqueued.ID {
				t.Fatal(err)
			}
			if _, err = s.SetRunJobName(ctx, run.ID, "workflow"); err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec(`INSERT INTO run_cancellations(run_id,job_name,superseded_by,reason,created_at) VALUES(?,?,'','owner_restart',?)`, run.ID, "workflow", unixNano(now)); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err = s.RecoverOwnerState(ctx); err != nil {
					t.Fatal(err)
				}
			}
			current, err := s.Run(ctx, run.ID)
			if err != nil || current.Status != model.RunRunning || current.ID != run.ID || current.AdmissionSequence != run.AdmissionSequence || current.QueueSequence != run.QueueSequence || current.Credentialed != run.Credentialed || current.SupersededBy != "" {
				t.Fatalf("recovery changed attempt: %+v %v", current, err)
			}
			if pending, err := s.PendingRunCancellations(ctx); err != nil || len(pending) != 0 {
				t.Fatalf("legacy restart deletion remains: %+v %v", pending, err)
			}
			if _, err = s.CancelRun(ctx, model.RunCancelRequest{RunID: run.ID, Actor: "operator@host"}); err != nil {
				t.Fatal(err)
			}
			if err = s.RecoverOwnerState(ctx); err != nil {
				t.Fatal(err)
			}
			pending, err := s.PendingRunCancellations(ctx)
			if err != nil || len(pending) != 1 || pending[0].RunID != run.ID || pending[0].Reason != "superseded" || pending[0].JobName != "workflow" {
				t.Fatalf("explicit deletion was retired: %+v %v", pending, err)
			}
			if counts, err := s.CountRuns(ctx); err != nil || counts.Total != 1 {
				t.Fatalf("trigger re-fired: %+v %v", counts, err)
			}
		})
	}
}
