package app_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/app/contracttest"
	"github.com/oberthci/oberth/internal/app/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/service"
	servicecontract "github.com/oberthci/oberth/internal/service/contracttest"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

var _ service.Starter = (*app.Submitter)(nil)
var _ interface {
	Decide(context.Context, service.RefUpdate) (service.Decision, error)
} = (*service.Service)(nil)

func TestStarterMatchesServiceContractAndResolver(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	q := startRequest("starter-contract", request())
	servicecontract.Starter(t, s, q)
	contracttest.StartResolver(t, func(*testing.T) contracttest.StartResolverFixture {
		return contracttest.StartResolverFixture{Port: f.d.Resolver, Request: q, Expected: request()}
	})
}
func TestStarterPreparationFailureRecordsAndCreatesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := workflow()
	w.Spec.Templates[0].Container.Image = "unapproved/image:1"
	f.setDocument(w)
	accepts := 0
	f.runs.BeforeAccept = func(context.Context, stateport.BatchAcceptance) error { accepts++; return nil }
	out, err := f.new(t).StartRun(t.Context(), startRequest("bad-prepare", request()))
	if err == nil || out.ID != "" || accepts != 0 || f.creates.Load() != 0 || saved(f, "bad-prepare") {
		t.Fatal("preparation refusal caused effects", out, err, accepts)
	}
}
func TestStarterRerunGetsFreshSealAndPreservesAttributedRequest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	old := mustAdmit(t, s, f)
	accepted(t, f, old)
	if err := f.runs.FailUnbound(t.Context(), stateport.Action{Actor: request().Actor.Identity}, stateport.UnboundFailure{RunID: "r1", Reason: stateport.ReasonCreateRefused}); err != nil {
		t.Fatal(err)
	}
	w := workflow()
	w.Spec.Templates[0].Container.Image = "oberth.ci/build:fresh"
	f.setDocument(w)
	f.d.Resolver = fake.StartResolver{Resolve: func(ctx context.Context, q service.StartRequest) (app.Request, error) {
		r, err := resolveStart(ctx, q)
		r.RunID = "rerun"
		return r, err
	}}
	q := startRequest("new-effect", request())
	q.RerunOf = "r1"
	q.Reason = "use current pipeline authority"
	q.Request.Assist = true
	q.Request.Actor.Scope = []string{"origin"}
	out, err := f.new(t).StartRun(t.Context(), q)
	if err != nil || out.ID != "rerun" || out.State != "started" {
		t.Fatal(out, err)
	}
	run, ok := f.runs.Admission("rerun")
	if !ok {
		t.Fatal("rerun not saved")
	}
	assertRerunFacts(t, run, q)
	assertFreshRerun(t, f, old, run.Prepared.View())
	q.Request.Actor.Scope[0] = "changed"
	*q.Request.CommitSHA = "changed"
	detached, _ := f.runs.Admission("rerun")
	if detached.Request.Actor.Scope[0] != "origin" || *detached.Request.CommitSHA != request().Ref.CommitSHA {
		t.Fatal("accepted request aliases caller")
	}
}
func TestStarterObservationCancellationDoesNotResubmit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.native.CreateFunc = func(_ context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		f.creates.Add(1)
		close(entered)
		<-release
		return binding(p.View()), nil
	}
	s := f.new(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type reply struct {
		out apicontract.Receipt
		err error
	}
	done := make(chan reply, 1)
	go func() { out, err := s.StartRun(ctx, startRequest("observed", request())); done <- reply{out, err} }()
	<-entered
	cancel()
	got := <-done
	if got.err != nil || got.out.ID != "r1" || got.out.State != "accepted" {
		t.Fatal("observation cancelled durable work", got)
	}
	close(release)
	closeSubmitter(t, s)
	if f.prepares.Load() != 1 || f.creates.Load() != 1 {
		t.Fatal("observation resubmitted work")
	}
	if _, bound := f.runs.Workflow(t.Context(), "r1"); !bound {
		t.Fatal("accepted work did not finish")
	}
}
func TestGateRecordsThroughServiceWithoutRevalidatingTheSeal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	recorder := &recordingService{Service: f.runs}
	f.d.Service = recorder
	blocked := false
	load := f.authority.LoadFunc
	f.authority.LoadFunc = func(ctx context.Context, scope app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
		if blocked {
			t.Error("admission revalidated after seal")
			return admissioncontract.PrepareRequest{}, argoworkflow.Policy{}, errors.New("changed authority")
		}
		return load(ctx, scope)
	}
	s := f.new(t)
	r := f.gate(t, s)
	blocked = true
	f.runs.DecideFunc = func(context.Context, service.RefUpdate) (service.Decision, error) {
		t.Error("decision revalidated after seal")
		return service.Decision{}, errors.New("changed authority")
	}
	f.runs.BeforeAccept = func(_ context.Context, b stateport.BatchAcceptance) error {
		if len(b.Runs) != 1 || b.Runs[0].Prepared == nil || len(b.Runs[0].Actions) != 1 {
			t.Error("service did not receive complete sealed batch")
		}
		return nil
	}
	result, err := s.Deliver(t.Context(), r)
	if err != nil || result.Outcomes[0].State != "started" || f.prepares.Load() != 1 || f.creates.Load() != 1 || !recorder.called {
		t.Fatal(result, err)
	}
}

func assertRerunFacts(t *testing.T, run stateport.Acceptance, q service.StartRequest) {
	t.Helper()
	if run.RerunOf != "r1" || !run.Request.Assist || !reflect.DeepEqual(run.Request, q.Request) {
		t.Fatal("request facts changed", run)
	}
	if !strings.Contains(run.Actions[0].Details, q.Reason) {
		t.Fatal("caller reason was not audit detail", run.Actions)
	}
}
func assertFreshRerun(t *testing.T, f *fixture, old *admissioncontract.PreparedAdmission, v admissioncontract.PreparedView) {
	t.Helper()
	if v.Digest == old.View().Digest || v.Workflow.Spec.Templates[0].Container.Image != "oberth.ci/build:fresh" || f.prepares.Load() != 2 || f.creates.Load() != 1 {
		t.Fatal("rerun reused a seal", v.Digest)
	}
}

type recordingService struct {
	app.Service
	called bool
}

func (s *recordingService) Accept(ctx context.Context, b stateport.BatchAcceptance) (stateport.AcceptanceResult, error) {
	s.called = true
	return s.Service.Accept(ctx, b)
}

func TestStarterRerunIsRecordedByRealStore(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	db := realStore(t)
	f.d.Runs = db
	f.d.Service = storeService{Store: db, Policy: f.runs}
	old := mustAdmit(t, f.new(t), f)
	if _, err := db.AcceptBatch(t.Context(), batch(old, "original")); err != nil {
		t.Fatal(err)
	}
	if err := db.FailUnbound(t.Context(), stateport.Action{Actor: request().Actor.Identity}, stateport.UnboundFailure{RunID: "r1", Reason: stateport.ReasonCreateRefused}); err != nil {
		t.Fatal(err)
	}
	f.d.Resolver = fake.StartResolver{Resolve: func(ctx context.Context, q service.StartRequest) (app.Request, error) {
		r, err := resolveStart(ctx, q)
		r.RunID = "rerun"
		return r, err
	}}
	q := startRequest("new-receipt", request())
	q.RerunOf = "r1"
	q.Request.Assist = true
	q.Reason = "fresh admission"
	s := f.new(t)
	want := apicontract.Receipt{ID: "rerun", State: "started", Warning: "assist is not available in this version; running without it"}
	for range 2 {
		out, err := s.StartRun(t.Context(), q)
		if err != nil || out != want {
			t.Fatal(out, err)
		}
	}
	row, err := db.Run(t.Context(), "rerun")
	if err != nil {
		t.Fatal(err)
	}
	if row.Origin.RerunOf != "r1" || !row.Origin.AssistRequested || row.Origin.Assisted || row.Origin.WorkflowUID == "" || row.Admission.Digest == old.View().Digest {
		t.Fatal("real store lost rerun facts", row.Origin)
	}
}
