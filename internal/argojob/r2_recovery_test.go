package argojob_test

import (
	"context"
	"fmt"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
	"time"
)

// This probe exercises paging with an owner's bound row and a foreign unbound row.
// All restored values are read from the owner, never reconstructed by the probe.
type recoveredOwner interface{ argojob.Runs }

type recoveryPages struct {
	recoveredOwner
	row     stateport.Run
	reads   int
	filters []stateport.RunFilter
}

func (p *recoveryPages) Runs(_ context.Context, q stateport.RunFilter) ([]stateport.Run, error) {
	p.filters = append(p.filters, q)
	if q.Status != stepmodel.RunQueued {
		return nil, nil
	}
	if q.Cursor != "" {
		if q.Cursor != p.row.ID {
			return nil, fmt.Errorf("wrong cursor %q", q.Cursor)
		}
		return []stateport.Run{p.row}, nil
	}
	rows := make([]stateport.Run, q.Limit)
	for i := range rows {
		rows[i] = p.row
	}
	rows[0].Origin.WorkflowUID = ""
	rows[0].ID = "without-uid"
	return rows, nil
}
func (p *recoveryPages) Run(ctx context.Context, id string) (stateport.Run, error) {
	p.reads++
	return p.recoveredOwner.Run(ctx, id)
}
func TestRecoverySkipsUnboundReadsQueuedAndAllPages(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	row, err := c.Runs.Run(t.Context(), b.RunID)
	compositionOK(t, err)
	pages := &recoveryPages{recoveredOwner: c.Runs, row: row}
	c.Runs = pages
	f.Object.Status.Phase = wf.WorkflowSucceeded
	c.Cache = &fake.Cache{WorkflowObject: f.Object}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	c.Reporter = &reportSpy{FinishedFunc: func(context.Context, string, argojob.Observation) error { cancel(); return nil }}
	_ = argojob.New(c).Run(ctx)
	if pages.reads != 200 || len(pages.filters) != 3 || pages.filters[0].Status != stepmodel.RunQueued || pages.filters[1].Cursor != row.ID || pages.filters[2].Status != stepmodel.RunRunning {
		t.Fatal("incomplete recovery", pages.reads, pages.filters)
	}
}
