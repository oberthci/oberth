package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestArtifactDeadlineSurvivesPublicationRetryAndSkip(t *testing.T) {
	for _, skip := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit-publication-retry", true: "deleted-branch"}[skip], func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/collection", strings.Repeat("a", 40)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ClaimNextRun(t.Context()); err != nil {
				t.Fatal(err)
			}
			pub, err := s.BeginPublication(t.Context(), model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor, ArtifactCollectionDeadline: true})
			if err != nil {
				t.Fatal(err)
			}
			if !skip {
				failed, err := s.FinalizePublication(t.Context(), pub.ID, model.PublicationFailed, "permission denied")
				if err != nil || failed.Run.Error != "permission denied\n"+model.ArtifactCollectionDeadlineReason {
					t.Fatal(failed, err)
				}
				if _, err = s.RetryFailedPublication(t.Context(), run.ID, "", run.Actor); err != nil {
					t.Fatal(err)
				}
				current, err := s.Run(t.Context(), run.ID)
				if err != nil || current.Error != model.ArtifactCollectionDeadlineReason {
					t.Fatal(current, err)
				}
			}
			var final model.PublicationFinalization
			if skip {
				final, err = s.SkipDeletedBranchPublication(t.Context(), pub.ID, "operator", "branch deleted")
			} else {
				final, err = s.FinalizePublication(t.Context(), pub.ID, model.PublicationDelivered, "")
			}
			if err != nil || final.Run.Status != model.RunPassed || final.Run.Error != model.ArtifactCollectionDeadlineReason {
				t.Fatal(final, err)
			}
			if _, err = s.VerifyAuditState(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
