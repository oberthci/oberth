package service_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestHTTPViewCountsAreCompleteAndScoped(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	for i := 1; i < stateport.RunPageLimit+5; i++ {
		r.ID = fmt.Sprintf("%032x", i)
		f.store.native.SeedRun(r)
	}
	r.ID = "ffffffffffffffffffffffffffffffff"
	r.Repo = "private/team/hidden"
	f.store.native.SeedRun(r)
	prepared, err := f.s.Views().Counts.Handler.Prepare(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.Empty{})
	value, err := execute(t.Context(), prepared, err)
	if err != nil || value.Running != stateport.RunPageLimit+5 || value.Queued != 0 || value.Passed != 0 {
		t.Fatal("partial or unscoped counts", value, err)
	}
}
func TestHTTPContainerViewsReuseAuthorizedLogOwner(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	reads := 0
	f.logs.ReadFunc = func(_ context.Context, run stateport.Run, q apicontract.RunLogsArgs) (apicontract.LogResult, error) {
		reads++
		if run.ID != r.ID {
			t.Error("log view changed run identity")
		}
		if q.Containers {
			return apicontract.LogResult{Containers: []apicontract.Container{{NodeID: "node", Container: "main"}}}, nil
		}
		if q.NodeID != "node" || q.Container != "main" {
			t.Error("container log selector changed")
		}
		return apicontract.LogResult{Content: "retained output"}, nil
	}
	views := f.s.Views()
	who := apicontract.Caller{Actor: caller(), CredentialID: "credential"}
	call, err := views.Containers.Handler.Prepare(t.Context(), who, apicontract.IDArgs{ID: r.ID})
	page, err := execute(t.Context(), call, err)
	if err != nil || len(page.Containers) != 1 {
		t.Fatal("container inventory missing", page, err)
	}
	log, err := views.ContainerLog.Handler.Prepare(t.Context(), who, apicontract.ContainerArgs{ID: r.ID, NodeID: "node", Container: "main"})
	result, err := execute(t.Context(), log, err)
	if err != nil || result.Content != "retained output" || reads != 2 {
		t.Fatal("container log missing", result, err)
	}
	r.Repo = "private/team/hidden"
	f.store.native.SeedRun(r)
	_, err = views.Containers.Handler.Prepare(t.Context(), who, apicontract.IDArgs{ID: r.ID})
	requireCode(t, err, publicerr.NotFound)
	_, err = views.ContainerLog.Handler.Prepare(t.Context(), who, apicontract.ContainerArgs{ID: r.ID, NodeID: "node", Container: "main"})
	requireCode(t, err, publicerr.NotFound)
	if reads != 2 {
		t.Fatal("hidden run reached log owner")
	}
}
