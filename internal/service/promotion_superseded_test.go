package service

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

type promotionRetryLog struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *promotionRetryLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}
func (b *promotionRetryLog) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.data.String() }

func TestStartupSkipsPromotionAlreadyDeliveredByAnotherPromotion(t *testing.T) {
	git := &scriptedPromotionGit{plans: []gitcache.MergeCandidate{{BaseSHA: retryBaseSHA, MergedSHA: retryMergedSHA}, {BaseSHA: retryBaseSHA, MergedSHA: retryMerged2}}}
	f, scheduler, control, _ := newPromotionRetryHarnessWithUplinks(t, git, passedPromotionCI)
	actor := api.Actor{Identity: "admin@host", Admin: true}
	id := promoteForRetry(t, control, actor)
	original, err := f.store.Promotion(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.ClaimNextRun(f.store, t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.MarkRunSubmissionStarted(t.Context(), original.RunID); err != nil {
		t.Fatal(err)
	}
	if err = f.store.RecoverOwnerState(t.Context()); err != nil {
		t.Fatal(err)
	}
	deliveredID := promoteForRetry(t, control, actor)
	if err = scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if delivered, err := f.store.Promotion(t.Context(), deliveredID); err != nil || delivered.Status != model.PromotionPassed {
		t.Fatal("second promotion did not deliver", delivered, err)
	}

	var output promotionRetryLog
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	if lineage, err := f.store.PromotionLineage(t.Context(), id); err != nil || lineage.RetriedAs != "" {
		t.Fatal("created unnecessary retry", lineage, err)
	}
	if prepared, pushed := git.counts(); prepared != 2 || pushed != 1 {
		t.Fatal("startup repeated delivered work", prepared, pushed)
	}
	// Exercise the retry transaction race directly: delivery can happen after startup
	// eligibility was read. The transaction's typed refusal must stay quiet.
	original, err = f.store.Promotion(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	control.retryAutomaticPromotion(t.Context(), original, "target moved")
	if strings.Contains(output.String(), "automatic retry of promotion "+id) {
		t.Fatal("delivered promotion logged retry failure", output.String())
	}
	_, _, _, err = control.admitPromotionRetry(t.Context(), actor, original, model.PromotionRetryAuto)
	if !errors.Is(err, store.ErrPromotionAlreadyDelivered) || !errors.Is(err, store.ErrInvalidState) {
		t.Fatal("delivery race lost typed refusal", err)
	}
}
