package service_test

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

func scheduledPageFixture(t *testing.T) (*fixture, []string) {
	t.Helper()
	f := realEventsFixture(t)
	older := acceptLiveRun(t, f, "refs/heads/older")
	child := acceptScheduledRun(t, f, repository)
	newer := acceptLiveRun(t, f, "refs/heads/newer")
	return f, []string{newer.ID, child.ID, older.ID}
}

func assertScheduledPage(t *testing.T, page apicontract.RunPage, ids []string, cursor string) {
	t.Helper()
	if len(page.Runs) != len(ids) || page.NextCursor != cursor {
		t.Fatal("scheduled page boundary", page, ids, cursor)
	}
	for i, id := range ids {
		if page.Runs[i].ID != id {
			t.Fatal("scheduled page duplicated or skipped a run", page, ids)
		}
	}
}

func TestScheduledRunCursorContinuesRealStoreHistory(t *testing.T) {
	t.Parallel()
	f, ids := scheduledPageFixture(t)
	first, err := f.s.RunList(t.Context(), caller(), viewQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertScheduledPage(t, first, ids[:2], ids[1])
	second, err := f.s.RunList(t.Context(), caller(), viewQuery{Limit: 2, Before: first.NextCursor})
	if err != nil {
		t.Fatal("scheduled cursor rejected", err)
	}
	assertScheduledPage(t, second, ids[2:], "")
}

type countedScheduledPages struct {
	service.RunStore
	calls *int
}

func (r countedScheduledPages) Runs(ctx context.Context, f stateport.RunFilter) ([]stateport.Run, error) {
	*r.calls++
	return r.RunStore.Runs(ctx, f)
}

func TestScheduledRunCursorRefusesMalformedBeforeRunLookup(t *testing.T) {
	t.Parallel()
	f, ids := scheduledPageFixture(t)
	calls := 0
	f.c.Runs = countedScheduledPages{RunStore: f.c.Runs, calls: &calls}
	f.restart(t)
	for _, cursor := range []string{"not-a-run", ids[1] + "-extra", ids[1] + "/other"} {
		call, err := f.s.Service.PrepareRunList(t.Context(), workerCaller(), apicontract.RunListArgs{Before: cursor})
		if call != nil {
			t.Fatal("malformed cursor prepared a query", cursor)
		}
		requireField(t, err, "before")
	}
	if calls != 0 {
		t.Fatal("malformed cursor reached run history", calls)
	}
	_, err := f.s.RunList(t.Context(), caller(), viewQuery{Before: ids[1][:len(ids[1])-1]})
	requireCode(t, err, publicerr.NotFound)
	if calls != 1 {
		t.Fatal("canonical cursor prefix bypassed scoped history", calls)
	}
}
