package api_test

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type progressPage struct {
	store   *statefake.MovingRuns
	id      string
	mu      sync.Mutex
	ready   chan struct{}
	waiting bool
}

// The browser drives native facts; the real event service supplies every wakeup.
func progressExample(t *testing.T) http.Handler {
	store := statefake.WithMoving(statefake.NewRuns(testNow))
	store.SeedRepository(stateport.Repository{ID: 1, Upstream: "origin", Name: "web", Canonical: "origin/team/web"})
	p := &progressPage{store: store, id: strings.Repeat("c", 32), ready: make(chan struct{})}
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "prepare", Name: "prepare"}, {Path: "test", Name: "test"}}}
	store.SeedRun(stateport.Run{HasAdmission: true, RepositoryID: 1, RunSummary: stepmodel.RunSummary{ID: p.id, Repo: "origin/team/web", Ref: "refs/heads/progress", Status: stepmodel.RunQueued, CreatedAt: testNow()}, Admission: admissioncontract.PreparedView{Plan: plan}})
	store.SeedSteps(p.id, []stepmodel.RunStep{{Path: "prepare", Name: "prepare", Status: stepmodel.StepQueued, Detail: "Prepare diagnostics."}, {Path: "test", Name: "test", Status: stepmodel.StepPending}})
	bindings := exampleRunService(t, store).Bindings()
	f := setup(t, func(c *api.Config, b *apicontract.Bindings) {
		c.Runtime.Now = testNow
		c.Auth = exampleAuth{Authenticator: c.Auth, token: os.Getenv("OBERTH_UI_TOKEN")}
		b.RunGet, b.RunList = bindings.RunGet, bindings.RunList
		b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Repositories: []apicontract.Repository{{Repo: "origin/team/web"}}}}
		b.EventsWait.Handler = p.events(bindings.EventsWait.Handler)
		b.RunLogs.Handler = &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Run: p.logs}
		b.RunHoldList.Handler = &fake.Handler[apicontract.RunHoldListArgs, apicontract.HoldPage]{Value: apicontract.HoldPage{Holds: []model.LiveHold{}}}
		b.SystemStatus.Handler = &fake.Handler[apicontract.SystemStatusArgs, apicontract.SystemStatus]{Value: apicontract.SystemStatus{Ready: true, Version: "1"}}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/example/progress" {
			f.server.Handler().ServeHTTP(w, r)
			return
		}
		if err := p.advance(r.Context(), r.URL.Query().Get("phase")); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (p *progressPage) events(handler apicontract.Handler[apicontract.EventsWaitArgs, apicontract.EventResult]) apicontract.Handler[apicontract.EventsWaitArgs, apicontract.EventResult] {
	return apicontract.HandlerFunc[apicontract.EventsWaitArgs, apicontract.EventResult](func(ctx context.Context, c apicontract.Caller, q apicontract.EventsWaitArgs) (apicontract.Call[apicontract.EventResult], error) {
		call, err := handler.Prepare(ctx, c, q)
		p.mu.Lock()
		if !p.waiting {
			close(p.ready)
			p.waiting = true
		}
		p.mu.Unlock()
		return call, err
	})
}

func (p *progressPage) advance(ctx context.Context, phase string) error {
	p.mu.Lock()
	ready := p.ready
	p.mu.Unlock()
	select {
	case <-ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.Lock()
	p.ready, p.waiting = make(chan struct{}), false
	p.mu.Unlock()
	at := testNow()
	steps, err := p.store.Steps(ctx, p.id)
	if err != nil {
		return err
	}
	if err := p.store.SaveProgress(ctx, p.id, stateport.Progress{Steps: progressSteps(steps, phase, at)}); err != nil {
		return err
	}
	if phase != "passed" {
		return nil
	}
	run, err := p.store.Run(ctx, p.id)
	if err != nil {
		return err
	}
	event := stateport.EventWrite{Dedupe: "finished:" + p.id, Event: model.AgentEvent{Kind: string(model.EventRunFinished), Repo: run.Repo, RunID: run.ID, At: at, Summary: "passed"}}
	return p.store.Finish(ctx, stateport.Action{Actor: "operator", Events: []stateport.EventWrite{event}}, stateport.RunUpdate{ID: p.id, Revision: run.Revision, Status: stepmodel.RunPassed})
}

func progressSteps(steps []stepmodel.RunStep, phase string, at time.Time) []stepmodel.RunStep {
	steps[0].Status = stepmodel.StepRunning
	if steps[0].StartedAt == nil {
		steps[0].StartedAt = &at
	}
	steps[1].Status, steps[1].Detail = stepmodel.StepQueued, phase
	if phase != "running" {
		steps[0].Status = stepmodel.StepPassed
		if steps[0].FinishedAt == nil {
			steps[0].FinishedAt = &at
		}
		steps[1].Status = stepmodel.StepRunning
		if steps[1].StartedAt == nil {
			steps[1].StartedAt = &at
		}
	}
	if phase == "passed" {
		steps[1].Status, steps[1].FinishedAt = stepmodel.StepPassed, &at
	}
	if phase == "detail" {
		steps[0].Detail = "Updated prepare diagnostics."
	}
	return steps
}

func (p *progressPage) logs(ctx context.Context) (apicontract.LogResult, error) {
	steps, err := p.store.Steps(ctx, p.id)
	content := "prepare finished\n"
	if len(steps) > 0 && steps[0].Status == stepmodel.StepRunning {
		content = "prepare live output\n"
	}
	return apicontract.LogResult{Content: content, Meta: logcontract.Meta{}}, err
}
