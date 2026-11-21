package app_test

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/stateport"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestKeepBeforeCreate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	order := []string{}
	f.runs.BeforeAccept = func(context.Context, stateport.BatchAcceptance) error { order = append(order, "accept"); return nil }
	f.keeper.KeepFunc = func(context.Context, string, []sourcecontract.Binding) error {
		order = append(order, "keep")
		return nil
	}
	f.native.CreateFunc = func(ctx context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		order = append(order, "create")
		return binding(p.View()), nil
	}
	f.runs.BeforeBind = func(context.Context, stateport.Action, nativebinding.RunBinding) error {
		order = append(order, "bind")
		return nil
	}
	if _, err := f.new(t).StartRun(context.Background(), startRequest("order", request())); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"accept", "keep", "create", "bind"}) {
		t.Fatal(order)
	}
}
func TestKeepAndAcceptanceFailuresForbidCreate(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"accept", "keep"} {
		t.Run(stage, func(t *testing.T) { checkStageFailure(t, stage) })
	}
}
func checkStageFailure(t *testing.T, stage string) {
	t.Helper()
	f := newFixture(t)
	fail := errors.New("storage unavailable")
	if stage == "accept" {
		f.runs.BeforeAccept = func(context.Context, stateport.BatchAcceptance) error { return fail }
	} else {
		f.keeper.KeepFunc = func(context.Context, string, []sourcecontract.Binding) error { return permanentRetention(fail.Error()) }
	}
	out, err := f.new(t).StartRun(context.Background(), startRequest(stage, request()))
	if stage == "accept" && err == nil {
		t.Fatal("acceptance failure hidden")
	}
	if stage == "keep" && (err != nil || out.State != "failed") {
		t.Fatal("saved start failure was not acknowledged", out, err)
	}
	if f.creates.Load() != 0 {
		t.Fatal("created after failed " + stage)
	}
	if stage == "keep" {
		failure, ok := f.runs.Failure(t.Context(), "r1")
		if !ok || failure.Reason != "source retention refused" || failure.Detail != fail.Error() {
			t.Fatal(failure, ok)
		}
	}
}
func TestKeepReceivesExactSealedBindings(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := workflow()
	addSource(w, sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main})
	f.setDocument(w)
	s := f.new(t)
	r := receive()
	r.Updates[0].Request.Ref.Name = "refs/tags/demo"
	r.Updates[0].Request.Trigger = "tag"
	r.Source = f.source
	if _, err := s.Gate(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Deliver(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	run, _ := f.runs.Admission("r1")
	bindings := run.Prepared.View().SourceBindings
	if !reflect.DeepEqual(f.keeper.Bindings("r1"), bindings) {
		t.Fatal("C3 computed/replaced bindings")
	}
	b := bindings[0]
	if b.Export.Ref.ObjectSHA != request().Ref.ObjectSHA || b.Export.Ref.CommitSHA != request().Ref.CommitSHA || b.Export.DefaultBranch.CommitSHA != strings.Repeat("d", 40) {
		t.Fatal("checkout facts dropped", b)
	}
}

type recoveryCase struct {
	name                                     string
	found, readError, foreign, bound, failed bool
}

func TestKeepBeforeCreateAndCrashRetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []recoveryCase{
		{name: "accept_before_keep", failed: true}, {name: "keep_before_create", failed: true},
		{name: "create_before_bind", found: true, bound: true}, {name: "read_error", readError: true}, {name: "foreign", foreign: true, failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) { runRecoveryCase(t, tc) })
	}
}
func TestRecoverIgnoresAlreadyBoundRuns(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := workflow()
	addSource(w, sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main})
	f.setDocument(w)
	finds := 0
	f.native.FindFunc = func(_ context.Context, run stateport.Run) (nativebinding.RunBinding, bool, error) {
		finds++
		return nativebinding.RunBinding{}, false, nil
	}
	s := f.new(t)
	if _, err := s.StartRun(context.Background(), startRequest("bound", request())); err != nil {
		t.Fatal(err)
	}
	s = f.new(t)
	if err := s.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if finds != 0 || f.creates.Load() != 1 || f.prepares.Load() != 1 {
		t.Fatal("bound run replayed")
	}
}
func TestStopIntentDuringCreate(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"Stop", "Terminate"} {
		t.Run(map[string]string{"Stop": "stop", "Terminate": "terminate"}[mode], func(t *testing.T) {
			f := newFixture(t)
			stopped := false
			f.native.CreateFunc = func(ctx context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
				f.runs.SetStop(ctx, "r1", mode)
				if mode == "Terminate" {
					f.runs.SetStop(ctx, "r1", "Stop")
				}
				return binding(p.View()), nil
			}

			shutdown := func(ctx context.Context, b nativebinding.RunBinding) error {
				stopped = true
				if _, ok := f.runs.Workflow(ctx, b.RunID); !ok {
					t.Error("stop before native binding")
				}
				return nil
			}
			wrong := func(context.Context, nativebinding.RunBinding) error { t.Error("wrong stop mode"); return nil }
			f.native.StopFunc, f.native.TerminateFunc = shutdown, wrong
			if mode == "Terminate" {
				f.native.StopFunc, f.native.TerminateFunc = wrong, shutdown
			}

			if _, err := f.new(t).StartRun(context.Background(), startRequest("stop", request())); err != nil {
				t.Fatal(err)
			}
			if !stopped {
				t.Fatal("stop intent lost")
			}
		})
	}
}
func TestClusterRefusalKeepsCauseWithoutRecoveryOrRerun(t *testing.T) {
	t.Parallel()
	for _, cause := range []string{"quota exceeded", "webhook refused the pod", "invalid native spec"} {
		t.Run(cause, func(t *testing.T) {
			f := newFixture(t)
			finds := 0
			f.native.CreateFunc = func(_ context.Context, _ *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
				f.creates.Add(1)
				return nativebinding.RunBinding{}, permanentCreate(cause)
			}
			f.native.FindFunc = func(_ context.Context, run stateport.Run) (nativebinding.RunBinding, bool, error) {
				finds++
				return nativebinding.RunBinding{}, false, nil
			}
			out, err := f.new(t).StartRun(context.Background(), startRequest("refused", request()))
			if err != nil || out.State != "failed" || out.Warning == "" {
				t.Fatal("saved cluster refusal not acknowledged", out, err)
			}
			failure, ok := f.runs.Failure(t.Context(), "r1")
			if !ok || failure.Reason != "workflow creation refused" || failure.Detail != cause {
				t.Fatal(failure, ok)
			}
			if finds != 0 || f.creates.Load() != 1 || strings.Contains(out.Warning, "rerun") {
				t.Fatal("second settlement or misleading remedy", finds, err)
			}
		})
	}
}

func runRecoveryCase(t *testing.T, tc recoveryCase) {
	t.Helper()
	f := newFixture(t)
	w := workflow()
	addSource(w, sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main})
	f.setDocument(w)
	finds := 0
	f.native.FindFunc = func(_ context.Context, run stateport.Run) (nativebinding.RunBinding, bool, error) {
		finds++
		return tc.lookup(run.Admission)
	}
	s := f.new(t)
	p := mustAdmit(t, s, f)
	accepted(t, f, p)
	if tc.name == "keep_before_create" {
		if err := f.keeper.Keep(context.Background(), "r1", p.View().SourceBindings); err != nil {
			t.Fatal(err)
		}
	}
	keeps := 0
	f.keeper.KeepFunc = func(context.Context, string, []sourcecontract.Binding) error { keeps++; return nil }
	err := s.Recover(context.Background())
	if (err != nil) != tc.readError {
		t.Fatal(err)
	}
	assertRecovery(t, f, tc, finds)
	if tc.bound && (keeps != 1 || !reflect.DeepEqual(f.keeper.Bindings("r1"), p.View().SourceBindings)) {
		t.Fatal("adoption did not keep the exact sealed source bindings")
	}
	failure, _ := f.runs.Failure(t.Context(), "r1")
	if tc.failed && !tc.foreign && failure.Reason != "workflow absent after restart" {
		t.Fatal(failure)
	}
}

func (tc recoveryCase) lookup(v admissioncontract.PreparedView) (nativebinding.RunBinding, bool, error) {
	if tc.foreign {
		return nativebinding.RunBinding{}, false, &nativebinding.Refusal{}
	}
	if tc.readError {
		return nativebinding.RunBinding{}, false, errors.New("reader unavailable")
	}
	return binding(v), tc.found, nil
}
func assertRecovery(t *testing.T, f *fixture, tc recoveryCase, finds int) {
	t.Helper()
	_, bound := f.runs.Workflow(t.Context(), "r1")
	_, failed := f.runs.Failure(t.Context(), "r1")
	if finds != 1 || bound != tc.bound || failed != tc.failed || f.creates.Load() != 0 || f.prepares.Load() != 1 {
		t.Fatal("wrong recovery handoff", bound, failed, finds)
	}
}

func TestShutdownDuringKeepOrCreateLeavesRunForRecovery(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"keep", "create"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t)
			lifetime, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.d.Lifetime = lifetime
			entered := make(chan struct{})
			failed := false
			f.runs.BeforeFail = func(context.Context, string) error { failed = true; return nil }
			if stage == "keep" {
				f.keeper.KeepFunc = func(ctx context.Context, _ string, _ []sourcecontract.Binding) error {
					close(entered)
					<-ctx.Done()
					return ctx.Err()
				}
			} else {
				f.native.CreateFunc = func(ctx context.Context, _ *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
					close(entered)
					<-ctx.Done()
					return nativebinding.RunBinding{}, ctx.Err()
				}
			}
			s := f.new(t)
			done := make(chan error, 1)
			go func() { _, err := s.StartRun(context.Background(), startRequest("cancelled", request())); done <- err }()
			<-entered
			cancel()
			if err := <-done; err != nil {
				t.Fatal("saved cancellation was not acknowledged", err)
			}
			closeSubmitter(t, s)
			if failed {
				t.Fatal("shutdown cancellation was recorded as a run failure")
			}
			if _, ok := f.runs.Failure(t.Context(), "r1"); ok {
				t.Fatal("cancelled run cannot recover")
			}
			rows, err := f.runs.UnboundRuns(context.Background(), 0, 200)
			if err != nil || len(rows) != 1 {
				t.Fatal(rows, err)
			}
		})
	}
}

func TestRecoveryPagesBySavedSequenceAndReportsReadOutage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	acceptRecoveryRows(t, f, s)
	outage := errors.New("recovery reader unavailable")
	calls := 0
	f.runs.BeforeUnbound = func(_ context.Context, before int64, limit int) error {
		calls++
		if limit != 200 {
			t.Fatal("invalid owner page limit", limit)
		}
		if calls == 1 {
			if before != 0 {
				t.Fatal("first page not at head", before)
			}
			return nil
		}
		if before != 1 {
			t.Fatal("cursor not last descending sequence", before)
		}
		return outage
	}
	if err := s.Recover(t.Context()); !errors.Is(err, outage) {
		t.Fatal("read outage became empty page", err)
	}
	if calls != 2 || f.creates.Load() != 0 {
		t.Fatal("recovery refired or did not page", calls)
	}
	for _, id := range []string{"first", "second"} {
		if _, ok := f.runs.Workflow(t.Context(), id); !ok {
			t.Fatal("page lost run", id)
		}
	}
}

func acceptRecoveryRows(t *testing.T, f *fixture, s *app.Submitter) {
	t.Helper()
	for _, id := range []string{"first", "second"} {
		r := request()
		r.RunID = id
		p, err := s.Admit(t.Context(), r, f.source)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.runs.AcceptBatch(t.Context(), batch(p, "receive-"+id)); err != nil {
			t.Fatal(err)
		}
	}
}
