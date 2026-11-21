package service_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport/contracttest"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestMovingStoreConsumerContracts(t *testing.T) {
	t.Parallel()
	t.Run("publication", func(t *testing.T) {
		f := setup(t)
		f.seed(stepmodel.RunPassed)
		contracttest.PublicationStore(t, f.store, publication().Publication)
	})
	t.Run("promotion", func(t *testing.T) {
		f := setup(t)
		contracttest.PromotionStore(t, f.store, model.Promotion{ID: "contract", Repo: repository, Actor: caller().Identity, Caller: caller(), SourceSHA: head, ResultSHA: head, TargetRef: "refs/heads/main", Status: model.PromotionPending})
	})
	t.Run("hold", func(t *testing.T) {
		f := heldFixture(t)
		r, err := f.store.Run(t.Context(), runID)
		if err != nil {
			t.Fatal(err)
		}
		contracttest.HoldStore(t, f.store, r)
	})
}
