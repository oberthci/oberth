package service

import (
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestRestartNeverResubmitsPromotion(t *testing.T) {
	git := &scriptedPromotionGit{plans: []gitcache.MergeCandidate{{BaseSHA: retryBaseSHA, MergedSHA: retryMergedSHA}}}
	f, scheduler, service, _ := newPromotionRetryHarnessWithUplinks(t, git, passedPromotionCI)
	actor := api.Actor{Identity: "agent@host", Fingerprint: "SHA256:agent"}
	if _, err := f.store.AppendPromotionGrant(t.Context(), model.PromotionGrantSpec{RepoID: f.repo.ID, SourceSHA: retrySourceSHA, TargetRef: "main", Issuer: "admin@host", IssuerFingerprint: "SHA256:admin", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	id := promoteForRetry(t, service, actor)
	service.waitBackground()
	original, err := f.store.Promotion(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.ClaimNextRun(f.store, t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = f.store.RecoverOwnerState(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = scheduler.ProcessNext(t.Context()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	current, err := f.store.Promotion(t.Context(), id)
	if err != nil || current.Status != model.PromotionFailed || current.Error != model.ServerRestartReason || current.RunID != original.RunID {
		t.Fatal(current, err)
	}
	lineage, err := f.store.PromotionLineage(t.Context(), id)
	if err != nil || lineage.RetriedAs != "" {
		t.Fatal(lineage, err)
	}
	prepared, pushed := git.counts()
	if prepared != 1 || pushed != 0 {
		t.Fatal("restart executed promotion again", prepared, pushed)
	}
}
