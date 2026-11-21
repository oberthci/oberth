package app_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/app/fake"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func TestStartRequiresStoredActionID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.d.Resolver = fake.StartResolver{Resolve: func(context.Context, service.StartRequest) (app.Request, error) {
		t.Error("resolved a start without its action ID")
		return request(), nil
	}}
	q := startRequest("", request())
	out, err := f.new(t).StartRun(t.Context(), q)
	expectRefusal(t, err, "request")
	if out.ID != "" || f.prepares.Load() != 0 || f.creates.Load() != 0 {
		t.Fatal(out, f.prepares.Load(), f.creates.Load())
	}
}
func TestRepeatedStartWithFreshRunIDsCreatesOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	sequence := 0
	f.d.Resolver = fake.StartResolver{Resolve: func(ctx context.Context, q service.StartRequest) (app.Request, error) {
		r, err := resolveStart(ctx, q)
		sequence++
		r.RunID = fmt.Sprintf("fresh%d", sequence)
		return r, err
	}}
	s := f.new(t)
	q := startRequest("stored-action", request())
	first, err := s.StartRun(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.StartRun(t.Context(), q)
	if err != nil || second.ID != first.ID || f.creates.Load() != 1 || f.prepares.Load() != 1 || sequence != 1 {
		t.Fatal(first, second, err, f.creates.Load())
	}
	rows, err := f.runs.Runs(t.Context(), stateport.RunFilter{Limit: 200})
	if err != nil || len(rows) != 1 || rows[0].Origin.WorkflowUID == "" {
		t.Fatal(rows, err)
	}
}
func TestRecoveryCannotFailAnInFlightStart(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	create := f.native.CreateFunc
	f.native.CreateFunc = func(ctx context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		close(entered)
		<-release
		return create(ctx, p)
	}
	f.native.FindFunc = func(context.Context, stateport.Run) (nativebinding.RunBinding, bool, error) {
		t.Error("recovery inspected a live start")
		return nativebinding.RunBinding{}, false, nil
	}
	s := f.new(t)
	done := make(chan error, 1)
	go func() { _, err := s.StartRun(t.Context(), startRequest("in-flight", request())); done <- err }()
	<-entered
	recoveryErr := s.Recover(t.Context())
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	expectRefusal(t, recoveryErr, "recovery")
	row, err := f.runs.Run(t.Context(), "r1")
	if err != nil || row.Origin.WorkflowUID == "" || row.Status == "failed" {
		t.Fatal("live Workflow was orphaned", row, err)
	}
}
func TestUnlabelledConnectionErrorStaysAcceptedForRecovery(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	failure := errors.New("connection lost")
	f.native.CreateFunc = func(context.Context, *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		return nativebinding.RunBinding{}, failure
	}
	out, err := f.new(t).StartRun(t.Context(), startRequest("connection-loss", request()))
	if err != nil || out.State != "accepted" || (!strings.Contains(out.Warning, "stays accepted") || !strings.Contains(out.Warning, "next server start")) || strings.Contains(out.Warning, "retry") {
		t.Fatal(out, err)
	}
	row, err := f.runs.Run(t.Context(), out.ID)
	if err != nil || row.Status == "failed" || row.Origin.WorkflowUID != "" {
		t.Fatal(row, err)
	}
}
func TestReleaseComesOnlyFromServiceDecision(t *testing.T) {
	t.Parallel()
	for _, file := range []string{workflowpolicy.ReleaseFile, ".oberth/publish.yaml", workflowpolicy.BuildFile, "workflows/release.yaml"} {
		for _, release := range []bool{false, true} {
			t.Run(fmt.Sprint(file, "/", release), func(t *testing.T) {
				checkReleaseServiceSelection(t, file, release)
			})
		}
	}
}

func checkReleaseServiceSelection(t *testing.T, file string, release bool) {
	t.Helper()
	f := newFixture(t)
	f.d.Routes = workflowpolicy.ReleaseRoutes{request().Repository: {{Pattern: "v1.*", File: file}}}
	f.runs.DecideFunc = func(_ context.Context, u service.RefUpdate) (service.Decision, error) {
		d := decision(u, "")
		d.Release = release
		return d, nil
	}
	r := receive()
	r.Source = f.source
	r.Updates[0].Request.Ref.Name = "refs/tags/v1.2.3"
	r.Updates[0].Request.Trigger = model.TriggerTag
	r.Updates[0].Request.Release = !release
	s := f.new(t)
	if _, err := s.Gate(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Deliver(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	a, ok := f.runs.Admission("r1")
	if !ok || a.Release != release {
		t.Fatal("caller selected release tier", a, release)
	}
	want := workflowpolicy.BuildFile
	if release {
		want = file
	}
	if a.Prepared.View().SelectedFile != want {
		t.Fatal("service selection was ignored", a.Prepared.View().SelectedFile, want)
	}
}

func TestReplayKeepsAnUnboundRunAccepted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.native.CreateFunc = func(context.Context, *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		return nativebinding.RunBinding{}, errors.New("connection lost")
	}
	s := f.new(t)
	r := f.gate(t, s)
	for range 2 {
		out, err := s.Deliver(t.Context(), r)
		if err != nil || len(out.Outcomes) != 1 || out.Outcomes[0].State != "accepted" || strings.Contains(strings.Join(out.Lines(), "\n"), "run started") {
			t.Fatal(out, err)
		}
	}
}
func TestAuditUsesTheFixedWorkflowName(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := request()
	r.RunID = "human-readable-run"
	out, err := f.new(t).StartRun(t.Context(), startRequest("fixed-audit", r))
	if err != nil {
		t.Fatal(err)
	}
	a, ok := f.runs.Admission(out.ID)
	if !ok || !strings.Contains(a.Actions[0].Details, `"workflow":"`+nativebinding.WorkflowName(out.ID)+`"`) {
		t.Fatal(a)
	}
}
func TestStoredRunsAreMatchedByID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	record := f.d.Service
	f.d.Service = &acceptReordered{Service: record}
	r := receive()
	second := r.Updates[0]
	second.Request.RunID = "r2"
	second.Request.Ref.Name = "refs/heads/second"
	r.Updates = append(r.Updates, second)
	r.Source = f.source
	f.native.CreateFunc = func(_ context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		return nativebinding.RunBinding{}, permanentCreate(p.View().RunID)
	}
	s := f.new(t)
	if _, err := s.Gate(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Deliver(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"r1", "r2"} {
		row, err := f.runs.Run(t.Context(), id)
		if err != nil || row.DisplayReason != stateport.ReasonCreateRefused || row.Detail != id {
			t.Fatal(row, err)
		}
	}
}

type acceptReordered struct{ app.Service }

func (s *acceptReordered) Accept(ctx context.Context, b stateport.BatchAcceptance) (stateport.AcceptanceResult, error) {
	out, err := s.Service.Accept(ctx, b)
	for l, r := 0, len(out.Runs)-1; l < r; l, r = l+1, r-1 {
		out.Runs[l], out.Runs[r] = out.Runs[r], out.Runs[l]
	}
	return out, err
}

func permanentCreate(detail string) error {
	e := publicerr.New(publicerr.Forbidden, "workflow refused", "Correct the Workflow.", errors.New(detail))
	e.Detail = detail
	return e
}

func TestPushPassesReceiveIDBeforeRecording(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.runs.BeforeAccept = func(_ context.Context, b stateport.BatchAcceptance) error {
		if b.ReceiveID != "receive1" || len(b.Runs) != 1 || b.Runs[0].Request.ReceiveID != b.ReceiveID {
			t.Error("receive ID left for storage to guess", b)
		}
		return nil
	}
	s := f.new(t)
	r := f.gate(t, s)
	if _, err := s.Deliver(t.Context(), r); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryNamesTheMissingOrForeignWorkflow(t *testing.T) {
	t.Parallel()
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			f := newFixture(t)
			f.native.FindFunc = func(context.Context, stateport.Run) (nativebinding.RunBinding, bool, error) {
				if foreign {
					return nativebinding.RunBinding{}, false, &nativebinding.Refusal{}
				}
				return nativebinding.RunBinding{}, false, nil
			}
			s := f.new(t)
			p := mustAdmit(t, s, f)
			accepted(t, f, p)
			if err := s.Recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			row, err := f.runs.Run(t.Context(), "r1")
			if err != nil || !strings.Contains(row.Detail, "runs/"+nativebinding.WorkflowName("r1")) || strings.Contains(row.Detail, "new run ID") || strings.Contains(row.Detail, "Rerun this accepted run.") {
				t.Fatal(row.DisplayReason, row.Detail, err)
			}
		})
	}
}

func TestDeletionIntentComesUnchangedFromServiceDecision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var provided *stateport.BranchDeletion
	var want stateport.BranchDeletion
	f.runs.DecideFunc = func(ctx context.Context, u service.RefUpdate) (service.Decision, error) {
		d, err := deletionDecision(ctx, u)
		d.Deletion.Publication.ID = "service-publication"
		provided = d.Deletion
		want = *provided
		want.Request, want.Publication = want.Request.Clone(), want.Publication.Clone()
		return d, err
	}
	f.runs.BeforeAccept = func(_ context.Context, b stateport.BatchAcceptance) error {
		if len(b.Deletions) != 1 || !reflect.DeepEqual(b.Deletions[0], want) {
			t.Error("wiring changed the service's publication intent", b.Deletions)
		}
		return nil
	}
	r := mixedReceive("owner-deletion", "")
	r.Updates = r.Updates[:1]
	r.Updates[0].Request.RunID = "caller-picked-id"
	r.Source = f.source
	s := f.new(t)
	if _, err := s.Gate(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	provided.Publication.ID = "changed"
	*provided.Publication.PreviousSHA = strings.Repeat("e", 40)
	provided.Request.Actor.Identity = "changed"
	if _, err := s.Deliver(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	p, err := f.runs.Publication(t.Context(), want.Publication.ID)
	if err != nil || p.ID != "service-publication" || p.PreviousSHA == nil || *p.PreviousSHA != request().Ref.ObjectSHA {
		t.Fatal(p, err)
	}
}

func TestStartPreparationHonorsCallerCancellation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, err := f.new(t).StartRun(ctx, startRequest("cancelled-before-preparation", request()))
	if !errors.Is(err, context.Canceled) || out.ID != "" || f.prepares.Load() != 0 || f.creates.Load() != 0 || saved(f, "cancelled-before-preparation") {
		t.Fatal(out, err)
	}
}
