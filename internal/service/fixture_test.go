package service_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	ports "github.com/oberthci/oberth/internal/service/contracttest"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

const repository = "origin/team/widget"
const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const runID = "abcdef01000000000000000000000001"

func at() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }
func caller() actor.Actor {
	return actor.Actor{Identity: "worker", AuthMethod: "key", Fingerprint: "key", Scope: []string{"origin"}}
}
func admin() actor.Actor {
	return actor.Actor{Identity: "owner", AuthMethod: "key", Fingerprint: "owner", Admin: true}
}

type fixture struct {
	moving  *testMovingGit
	holds   *testHolds
	timers  *testTimers
	seq     atomic.Int64
	offset  atomic.Int64
	errors  chan error
	s       *testService
	c       service.Config
	store   *fixtureStore
	git     *testGit
	source  *testSource
	exec    *testExec
	logs    *testLogs
	starter *testStarter
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	f.store = newFixtureStore(func() time.Time { return f.c.Now() })
	for _, a := range []actor.Actor{caller(), admin()} {
		f.store.Actors[a.Fingerprint] = a
		f.store.Policies[a.Fingerprint] = service.KeyPolicy{Agent: !a.Admin}
	}
	f.store.native.SeedRepository(stateport.Repository{ID: 1, Upstream: "origin", Name: "widget", Canonical: repository})
	f.source = &testSource{Default: func(context.Context, string) (string, error) { return "refs/heads/main", nil }}
	f.git = &testGit{Resolve: func(_ context.Context, _ string, ref string) (gitport.Ref, error) {
		return gitport.Ref{Name: ref, ObjectSHA: head, CommitSHA: head}, nil
	}, Release: func(context.Context, stateport.Repository, gitport.Ref) error { return nil }}
	f.exec = &testExec{CancelFunc: func(context.Context, stateport.Run, bool) error { return nil }, InspectFunc: func(_ context.Context, q service.CheckRequest) (apicontract.RunCheckResult, error) {
		return apicontract.RunCheckResult{OK: true, Repo: q.Request.Repo, Ref: q.Request.Ref}, nil
	}}
	f.logs = &testLogs{ReadFunc: func(context.Context, stateport.Run, apicontract.RunLogsArgs) (apicontract.LogResult, error) {
		return apicontract.LogResult{Content: "output", Meta: logcontract.Meta{Follow: true, NextByteOffset: 23}}, nil
	}, PurgeFunc: func(context.Context, stateport.Run) error { return nil }}
	f.starter = &testStarter{Start: func(context.Context, service.StartRequest) (apicontract.Receipt, error) {
		return apicontract.Receipt{ID: "new-run", State: "running"}, nil
	}}
	f.c = service.Config{Runs: f.store, Authority: f.store, Catalog: f.store, Events: f.store, Git: f.git, Source: f.source, Exec: f.exec, Starter: f.starter, Logs: f.logs, Now: at}
	f.c.Authority = ports.AuthorityFuncs{CurrentFunc: f.store.Current, KeyPolicyFunc: f.store.KeyPolicy}
	f.c.Git = ports.GitFuncs{ResolveRefFunc: f.git.ResolveRef, ListRefsFunc: f.git.ListRefs, ReleaseTagFunc: f.git.ReleaseTag}
	f.c.Exec = ports.ExecFuncs{CancelFunc: f.exec.Cancel, InspectFunc: f.exec.Inspect, LocksFunc: f.exec.Locks}
	f.c.Logs = ports.LogsFuncs{ReadFunc: f.logs.Read, PurgeFunc: f.logs.Purge}
	f.setupMoving(t)
	f.restart(t)
	return f
}
func (f *fixture) restart(t *testing.T) {
	t.Helper()
	s, err := service.New(t.Context(), f.c)
	if err != nil {
		t.Fatal(err)
	}
	f.s = &testService{s}
	t.Cleanup(func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
}
func (f *fixture) seed(status stepmodel.RunStatus) stateport.Run {
	commit := head
	r := stateport.Run{RunSummary: stepmodel.RunSummary{ID: runID, Repo: repository, Ref: "refs/heads/topic", ObjectSHA: head, CommitSHA: &commit, Status: status, CreatedAt: at()}, HasAdmission: true, Admission: admissioncontract.PreparedView{Plan: admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "build"}, {Path: "test"}}}}, Revision: 1, Origin: model.RunOrigin{Actor: caller().Identity, Trigger: model.TriggerBranch}}
	f.store.native.SeedRun(r)
	f.store.native.SeedSteps(r.ID, []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepPending}, {Path: "test", Name: "test", Status: stepmodel.StepPending}})
	return r
}
func batch(t *testing.T) stateport.BatchAcceptance {
	t.Helper()
	commit := head
	request := model.CIRequest{ReceiveID: "receive-1", Repo: repository, Ref: "refs/heads/topic", ObjectSHA: head, CommitSHA: &commit, Actor: caller(), Trigger: model.TriggerBranch}
	view := admissioncontract.PreparedView{Workflow: &wfv1.Workflow{}, Repository: repository, RunID: runID, Ref: request.Ref, ObjectSHA: head, CommitSHA: head, Trigger: model.TriggerBranch, Plan: admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "build", Name: "build"}}}}
	// This fixture stands for C4 output; production service never seals an admission.
	prepared, err := admissioncontract.Seal(view)
	if err != nil {
		t.Fatal(err)
	}
	action := model.AuditAction{Actor: caller().Identity, Action: "run.accept", ResourceType: "run", ResourceID: runID}
	return stateport.BatchAcceptance{ReceiveID: request.ReceiveID, Repository: repository, Actor: caller(), Runs: []stateport.Acceptance{{Request: request, Prepared: prepared, Actions: []model.AuditAction{action}, Events: []stateport.EventWrite{{Event: model.AgentEvent{Kind: "run.updated", Repo: repository, RunID: runID}, Dedupe: "accepted:" + runID}}}}}
}
func update() service.RefUpdate {
	return service.RefUpdate{ID: "receive-1", Repo: repository, Ref: "refs/heads/topic", ObjectSHA: head, CommitSHA: head, Actor: caller(), Run: true}
}
func requireCode(t *testing.T, err error, code publicerr.Code) {
	t.Helper()
	var p *publicerr.Error
	if !errors.As(err, &p) || p.Code() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}
func requireField(t *testing.T, err error, field string) {
	t.Helper()
	requireCode(t, err, publicerr.InvalidInput)
	if !strings.Contains(err.Error(), field) {
		t.Fatalf("missing field %s: %v", field, err)
	}
}
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatal("operation did not complete")
		var zero T
		return zero
	}
}

func publication() *stateport.PublicationIntent {
	return &stateport.PublicationIntent{Publication: model.Publication{ID: "publication", Repo: repository, Ref: "refs/heads/topic", RunID: runID, ObjectSHA: head, CommitSHA: new(head), PreviousSHA: new(""), Actor: caller().Identity, Status: model.PublicationPending}}
}

func (f *fixture) nextID() string { return fmt.Sprintf("%032x", f.seq.Add(1)) }
func (f *fixture) setupMoving(t *testing.T) {
	t.Helper()
	f.errors = make(chan error, 16)
	f.timers = &testTimers{}
	f.source.File = func(ctx context.Context, repo, path string) (gitport.Ref, gitport.File, error) {
		ref, err := f.source.DefaultBranch(ctx, repo)
		return gitport.Ref{Name: ref, ObjectSHA: strings.Repeat("b", 40), CommitSHA: strings.Repeat("b", 40)}, gitport.File{Path: path, Bytes: []byte("uplinks_may_promote: true\n")}, err
	}
	f.moving = &testMovingGit{
		Remote: func(_ context.Context, _ string, ref string) (gitport.Ref, error) {
			return gitport.Ref{Name: ref, ObjectSHA: strings.Repeat("b", 40), CommitSHA: strings.Repeat("b", 40)}, nil
		},
		Prepare: func(_ context.Context, q gitport.PromotionRequest) (gitport.Promotion, error) {
			return gitport.Promotion{Result: gitport.Ref{Name: q.Target.Name, ObjectSHA: q.Source.Commit, CommitSHA: q.Source.Commit}}, nil
		},
		Push: func(_ context.Context, p model.Publication) (gitport.PublicationReceipt, error) {
			commit := ""
			if p.CommitSHA != nil {
				commit = *p.CommitSHA
			}
			return gitport.PublicationReceipt{Repository: p.Repo, ActionReceipt: p.ID, Refs: []gitport.Ref{{Name: p.Ref, ObjectSHA: p.ObjectSHA, CommitSHA: commit}}}, nil
		},
	}

	f.holds = &testHolds{Stop: func(context.Context, stateport.Run, model.LiveHold) error { return nil }, Execute: func(context.Context, stateport.Run, model.LiveHold, apicontract.RunHoldExecArgs) (apicontract.ExecResult, error) {
		return apicontract.ExecResult{}, nil
	}}
	f.c.Publications, f.c.Promotions, f.c.MovingGit = f.store, f.store, ports.MovingGitFuncs{RemoteRefFunc: f.moving.RemoteRef, PreparePromotionFunc: f.moving.PreparePromotion, PublishFunc: f.moving.Publish}
	f.c.HoldStore, f.c.Holds = f.store, ports.HoldExecFuncs{ListFunc: f.holds.List, AbandonFunc: f.holds.Abandon, ExecFunc: f.holds.Exec}
	f.c.AfterFunc, f.c.NewID = f.timers.After, f.nextID
	f.c.Now = func() time.Time { return at().Add(time.Duration(f.offset.Load())) }
	f.c.OnError = func(err error) { f.errors <- err }
	ordinary := f.starter.Start
	f.starter.Start = func(ctx context.Context, q service.StartRequest) (apicontract.Receipt, error) {
		if q.PromotionID == "" {
			return ordinary(ctx, q)
		}
		p, err := f.c.Promotions.Promotion(ctx, q.PromotionID)
		if err != nil {
			return apicontract.Receipt{}, err
		}
		if q.Request.ReceiveID != "promotion:"+p.ID+":"+p.BaseSHA {
			t.Error("promotion child lacks stable action ID", q.Request.ReceiveID)
		}
		id := f.nextID()
		request := q.Request.Clone()
		prepared := ownerAdmission(t, id, request)
		_, err = f.s.Accept(ctx, stateport.BatchAcceptance{ReceiveID: request.ReceiveID, Repository: request.Repo, Actor: request.Actor, Runs: []stateport.Acceptance{{Request: request, Prepared: prepared, PromotionID: q.PromotionID}}})
		return apicontract.Receipt{ID: id, State: "running"}, err
	}
}
