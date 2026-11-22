package app_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/app"
	appfake "github.com/oberthci/oberth/internal/app/fake"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/service"
	servicecontract "github.com/oberthci/oberth/internal/service/contracttest"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

type movingFixture struct {
	*fixture
	git     *gittest.Fixture
	db      *store.Store
	wiring  *app.Submitter
	svc     *service.Service
	moving  *observedMoving
	starter *observedStarter
}

// newMovingFixture promotes a tested topic beside a default branch that moved
// after the topic was cut, so the promotion writes a merge and runs CI on it.
func newMovingFixture(t *testing.T) *movingFixture {
	t.Helper()
	return newMovingFixtureFrom(t, promotionGit(t, true))
}

// newForwardFixture promotes a tested topic cut from an unmoved default branch,
// which fast-forwards.
func newForwardFixture(t *testing.T) *movingFixture {
	t.Helper()
	return newMovingFixtureFrom(t, promotionGit(t, false))
}

func newMovingFixtureFrom(t *testing.T, g *gittest.Fixture) *movingFixture {
	t.Helper()
	f := &movingFixture{fixture: newFixture(t), git: g}
	f.db = f.runs.Store
	cache := f.git.Cache(t, nil)
	f.moving = &observedMoving{MovingGit: cache}
	forward := &serviceForward{}
	native := appfake.NewNative(f.db.Run, func(ctx context.Context, b nativebinding.RunBinding) error {
		return f.db.BindWorkflow(ctx, stateport.Action{Actor: request().Actor.Identity}, b)
	})
	native.Hook = f.native.Hook
	create := native.CreateFunc
	native.CreateFunc = func(ctx context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		f.creates.Add(1)
		return create(ctx, p)
	}
	d := f.d
	d.Source, d.Keeper, d.Runs, d.Service, d.Native = cache, cache, f.db, forward, native
	var sequence atomic.Int64
	d.Resolver = appfake.StartResolver{Resolve: func(_ context.Context, q service.StartRequest) (app.Request, error) {
		r := q.Request
		return app.Request{RepositoryID: 1, Repository: r.Repo, RunID: fmt.Sprintf("%032x", sequence.Add(1)), Ref: gitport.Ref{Name: r.Ref, ObjectSHA: r.ObjectSHA, CommitSHA: *r.CommitSHA}, Actor: r.Actor.Clone(), Trigger: r.Trigger}, nil
	}}
	var err error
	f.wiring, err = app.New(d)
	landedOK(t, err)
	t.Cleanup(func() { landedOK(t, f.wiring.Close(context.Background())) })
	f.starter = &observedStarter{Starter: f.wiring}
	c := newServicePorts(t, f.db, f.wiring)
	c.Publications, c.Promotions, c.HoldStore = f.db, f.db, f.db
	c.Starter, c.MovingGit = f.starter, f.moving
	c.Git = servicecontract.GitFuncs{ResolveRefFunc: cache.RemoteRef}
	f.svc, err = service.New(t.Context(), c)
	landedOK(t, err)
	forward.Service = f.svc
	t.Cleanup(func() { landedOK(t, f.svc.Shutdown(context.Background())) })
	return f
}

func promotionGit(t *testing.T, mainMoves bool) *gittest.Fixture {
	t.Helper()
	g := gittest.New(t)
	w := workflow()
	addSource(w, sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main})
	g.Write(t, workflowpolicy.BuildFile, document(w))
	g.Write(t, ".oberth/promotion.yaml", []byte("uplinks_may_promote: true\n"))
	g.CommitFile(t, "build and promotion policy")
	g.Git(t, g.Work, "push", g.Bare, "main")
	g.Git(t, g.Work, "checkout", "-b", "topic")
	g.Write(t, "feature", []byte("tested source\n"))
	g.Commit = g.CommitFile(t, "feature")
	g.Git(t, g.Work, "push", g.Bare, "topic")
	if mainMoves {
		g.Git(t, g.Work, "checkout", "main")
		g.Write(t, "main-change", []byte("default branch moved\n"))
		g.CommitFile(t, "move the default branch")
		g.Git(t, g.Work, "push", g.Bare, "main")
		g.Git(t, g.Work, "checkout", "topic")
	}
	return g
}

func (f *movingFixture) branch(t *testing.T, status stepmodel.RunStatus) apicontract.Receipt {
	t.Helper()
	r := model.CIRequest{ReceiveID: "branch-action", Repo: request().Repository, Ref: "refs/heads/topic", ObjectSHA: f.git.Commit, CommitSHA: new(f.git.Commit), Actor: movingCaller().Actor, Trigger: model.TriggerBranch}
	out, err := f.wiring.StartRun(t.Context(), service.StartRequest{Request: r})
	landedOK(t, err)
	f.finish(t, out.ID, status)
	return out
}

func (f *movingFixture) finish(t *testing.T, id string, status stepmodel.RunStatus) {
	t.Helper()
	landedOK(t, f.svc.Finished(t.Context(), service.FinishedRun{RunID: id, Status: status}))
}

func movingCaller() apicontract.Caller {
	a := request().Actor
	a.Fingerprint = "worker"
	return apicontract.Caller{Actor: a}
}

func (f *movingFixture) promote(t *testing.T) apicontract.Call[apicontract.PromoteResult] {
	t.Helper()
	call, err := f.svc.PreparePromote(t.Context(), movingCaller(), apicontract.PromoteArgs{Repo: request().Repository, SHA: f.git.Commit})
	landedOK(t, err)
	return call
}

type observedMoving struct {
	service.MovingGit
	mu       sync.Mutex
	requests []gitport.PromotionRequest
	receipts []gitport.PublicationReceipt
}

func (g *observedMoving) PreparePromotion(ctx context.Context, q gitport.PromotionRequest) (gitport.Promotion, error) {
	g.mu.Lock()
	g.requests = append(g.requests, q)
	g.mu.Unlock()
	return g.MovingGit.PreparePromotion(ctx, q)
}

func (g *observedMoving) Publish(ctx context.Context, p model.Publication) (gitport.PublicationReceipt, error) {
	r, err := g.MovingGit.Publish(ctx, p)
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil {
		g.receipts = append(g.receipts, r)
	}
	return r, err
}

type observedStarter struct {
	service.Starter
	mu       sync.Mutex
	requests []service.StartRequest
}

func (s *observedStarter) StartRun(ctx context.Context, q service.StartRequest) (apicontract.Receipt, error) {
	s.mu.Lock()
	s.requests = append(s.requests, q)
	s.mu.Unlock()
	return s.Starter.StartRun(ctx, q)
}
