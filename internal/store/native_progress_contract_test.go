package store

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/stateport/contracttest"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestNativeProgressStoreContract(t *testing.T) {
	t.Parallel()
	contracttest.Progress(t, func(t *testing.T) contracttest.ProgressFixture {
		s := testStore(t)
		r, _ := accepted(t, s, "progress")
		return contracttest.ProgressFixture{
			Store: s, RunID: r.ID, Path: "build",
			Revision: func(ctx context.Context) (int64, error) {
				got, err := s.Run(ctx, r.ID)
				return got.Revision, err
			},
			Steps: func(ctx context.Context) ([]stepmodel.RunStep, error) {
				return s.Steps(ctx, r.ID)
			},
		}
	})
}
