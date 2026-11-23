package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/service/contracttest"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// Read-only examples exercise the same service projection as the live run view.
func exampleSummaryRuns(t *testing.T, base apicontract.RunRecord) apicontract.Handler[apicontract.RunGetArgs, apicontract.RunRecord] {
	t.Helper()
	store := statefake.WithMoving(statefake.NewRuns(testNow))
	store.SeedRepository(stateport.Repository{ID: 1, Upstream: "origin", Name: "web", Canonical: base.Repo})
	for i, status := range []stepmodel.RunStatus{stepmodel.RunQueued, stepmodel.RunRunning, stepmodel.RunPassed, stepmodel.RunFailed, stepmodel.RunCancelled} {
		for _, specific := range []bool{false, true} {
			run := stateport.Run{RunSummary: base.Clone(), Origin: model.RunOrigin{RunCause: base.RunCause}}
			run.ID, run.Status, run.DisplayReason = fmt.Sprintf("a%030x%d", i, 0), status, ""
			if specific {
				run.ID, run.DisplayReason = fmt.Sprintf("b%030x%d", i, 0), "waiting for a node"
			}
			store.SeedRun(run)
			store.SeedSteps(run.ID, base.Steps)
		}
	}
	return exampleRunService(t, store).Bindings().RunGet.Handler
}

func exampleRunService(t *testing.T, store *statefake.MovingRuns) *service.Service {
	t.Helper()
	c := service.Config{Runs: store, Catalog: store, Events: store, Publications: store, Promotions: store, HoldStore: store,
		Authority: contracttest.AuthorityFuncs{CurrentFunc: func(_ context.Context, a actor.Actor) (actor.Actor, error) { return a, nil }},
		Git:       contracttest.GitFuncs{}, MovingGit: contracttest.MovingGitFuncs{}, Exec: contracttest.ExecFuncs{}, Logs: contracttest.LogsFuncs{}, Holds: contracttest.HoldExecFuncs{},
		Source: struct{ service.Source }{}, Starter: struct{ service.Starter }{}, Now: testNow,
		AfterFunc: func(d time.Duration, f func()) service.Timer { return time.AfterFunc(d, f) }, NewID: func() string { return strings.Repeat("f", 32) }, OnError: func(err error) { t.Error(err) }}
	s, err := service.New(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}
