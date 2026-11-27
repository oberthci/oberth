package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type viewReads struct {
	service.RunStore
	lookups int
}

func (r *viewReads) LookupRun(ctx context.Context, scope []string, id string) (stateport.Run, error) {
	r.lookups++
	return r.RunStore.LookupRun(ctx, scope, id)
}

func (*viewReads) ResolveRun(context.Context, []string, string) (stateport.Run, error) {
	return stateport.Run{}, errors.New("view requested the complete admission")
}

func TestRunGetSelectsMetadataWithoutAdmission(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	reads := &viewReads{RunStore: f.store}
	f.c.Runs = reads
	f.restart(t)
	for _, q := range []apicontract.RunGetArgs{
		{ID: r.ID}, {ID: r.ID[:8]},
	} {
		before := reads.lookups
		call, err := f.s.PrepareRunGet(t.Context(), workerCaller(), q)
		got, err := execute(t.Context(), call, err)
		if err != nil || got.ID != r.ID || len(got.Steps) != 2 || reads.lookups != before+2 {
			t.Fatal("run_get did not refresh its metadata selection", q, got, err, reads.lookups)
		}
	}
}
