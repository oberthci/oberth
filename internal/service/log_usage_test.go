package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/publicerr"
	ports "github.com/oberthci/oberth/internal/service/contracttest"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestLogUsageRunViewPreservesAuthority(t *testing.T) {
	f := setup(t)
	r := f.seed(stepmodel.RunPassed)
	calls := 0
	f.c.Logs = ports.LogsFuncs{ReadFunc: f.logs.Read, PurgeFunc: f.logs.Purge, UsageFunc: func(_ context.Context, got stateport.Run) (logcontract.Usage, error) {
		calls++
		if got.ID != r.ID {
			t.Fatal("foreign archive reached usage owner", got.ID)
		}
		return logcontract.Usage{PhysicalRunID: got.ID, Body: logcontract.StorageBytes{Logical: 123}, CaptureLimited: true}, nil
	}}
	f.restart(t)
	call, err := f.s.PrepareRunGet(t.Context(), workerCaller(), apicontract.RunGetArgs{ID: r.ID})
	got, err := execute(t.Context(), call, err)
	if err != nil || got.LogUsage == nil || got.LogUsage.PhysicalRunID != r.ID || got.LogUsage.Body.Logical != 123 || !got.LogUsage.CaptureLimited {
		t.Fatal(got, err)
	}
	hidden := r
	hidden.ID, hidden.Repo = strings.Repeat("b", 32), "foreign/team/hidden"
	f.store.native.SeedRepository(stateport.Repository{ID: 2, Upstream: "foreign", Name: "hidden", Canonical: hidden.Repo})
	f.store.native.SeedRun(hidden)
	_, err = f.s.PrepareRunGet(t.Context(), workerCaller(), apicontract.RunGetArgs{ID: hidden.ID})
	requireCode(t, err, publicerr.NotFound)
	if calls != 1 {
		t.Fatal("foreign metadata reached log usage owner")
	}
}
