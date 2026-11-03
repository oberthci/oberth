package service

import (
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"testing"
	"time"
)

func TestStartupRetriesInterruptedPromotionUnderOriginalGrant(t *testing.T) {
	for _, mode := range []string{"retry", "revoked", "failed-step", "removed-uplink", "manual-cancel"} {
		t.Run(mode, func(t *testing.T) {
			git := &scriptedPromotionGit{plans: []gitcache.MergeCandidate{{BaseSHA: retryBaseSHA, MergedSHA: retryMergedSHA}, {BaseSHA: retryBaseSHA, MergedSHA: retryMerged2}}}
			f, scheduler, service, uplinks := newPromotionRetryHarnessWithUplinks(t, git, passedPromotionCI)
			actor := api.Actor{Identity: "agent@host", Fingerprint: "SHA256:agent"}
			grant, err := f.store.AppendPromotionGrant(t.Context(), model.PromotionGrantSpec{RepoID: f.repo.ID, SourceSHA: retrySourceSHA, TargetRef: "main", Issuer: "admin@host", IssuerFingerprint: "SHA256:admin", ExpiresAt: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			id := promoteForRetry(t, service, actor)
			original, err := f.store.Promotion(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.ClaimNextRun(t.Context()); err != nil {
				t.Fatal(err)
			}
			if mode == "failed-step" {
				step := stepResult(model.StepFailed)
				step.RunID = original.RunID
				step.DeclaredSize = "S"
				if _, err = f.store.PutStepResult(t.Context(), step); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "manual-cancel" {
				if _, err = f.store.FinishRun(t.Context(), original.RunID, model.RunResult{Status: model.RunInterrupted, Phase: model.PhaseCancelled, Error: "operator canceled"}); err != nil {
					t.Fatal(err)
				}
			}
			if err = f.store.RecoverOwnerState(t.Context()); err != nil {
				t.Fatal(err)
			}
			if mode == "revoked" {
				if _, err = f.store.RevokePromotionGrant(t.Context(), grant.ID, "admin@host"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "removed-uplink" {
				uplinks.set(actor.Identity)
			}
			// A newer matching grant must never replace the original authority.
			if _, err = f.store.AppendPromotionGrant(t.Context(), model.PromotionGrantSpec{RepoID: f.repo.ID, SourceSHA: retrySourceSHA, TargetRef: "main", Issuer: "admin@host", IssuerFingerprint: "SHA256:admin", ExpiresAt: time.Now().Add(2 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if err = scheduler.recoverInterruptedPromotions(t.Context()); err != nil {
				t.Fatal(err)
			}
			service.waitBackground()
			lineage, err := f.store.PromotionLineage(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "retry" {
				if lineage.RetriedAs != "" {
					t.Fatalf("unsafe retry in %s: %+v", mode, lineage)
				}
				return
			}
			if lineage.RetriedAs == "" {
				t.Fatal("restart left promotion requiring manual retry")
			}
			next, err := f.store.Promotion(t.Context(), lineage.RetriedAs)
			if err != nil || next.GrantID != grant.ID || next.Status != model.PromotionPending {
				t.Fatalf("changed authority: %+v %v", next, err)
			}
			if err = scheduler.ProcessNext(t.Context()); err != nil {
				t.Fatal(err)
			}
			next, err = f.store.Promotion(t.Context(), next.ID)
			if err != nil || next.Status != model.PromotionPassed {
				t.Fatalf("retry did not deliver: %+v %v", next, err)
			}
			if err = scheduler.recoverInterruptedPromotions(t.Context()); err != nil {
				t.Fatal(err)
			}
			service.waitBackground()
			prepared, pushed := git.counts()
			if prepared != 2 || pushed != 1 {
				t.Fatalf("duplicate publication: plans=%d pushes=%v", prepared, pushed)
			}
		})
	}
}
