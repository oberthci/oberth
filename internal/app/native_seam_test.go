package app_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/app/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestNativePreparationReceivesExactFactsAndFixedName(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.d.Preparation = nativebinding.Preparation{HoldTTL: time.Hour, HelperImage: "oberth.ci/helper:1"}
	hook := f.native.Hook
	var preparation nativebinding.Preparation
	f.native.HookFunc = func(p nativebinding.Preparation) admissioncontract.PrepareHooks {
		preparation = p
		want := f.d.Preparation
		want.Repository, want.File, want.Assist = request().Repository, ".oberth/build.yaml", false
		want.RepositoryID, want.Ref, want.Trigger = request().RepositoryID, request().Ref, string(request().Trigger)
		if p != want {
			t.Errorf("native preparation changed: %+v", p)
		}
		return hook
	}
	create := f.native.CreateFunc
	f.native.CreateFunc = func(ctx context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		v := p.View()
		if v.Ref != preparation.Ref.Name || v.CommitSHA != preparation.Ref.CommitSHA || v.ObjectSHA != preparation.Ref.ObjectSHA {
			t.Error("native hook and sealed admission received different revisions")
		}
		b, err := create(ctx, p)
		if b.Workflow.Name != nativebinding.WorkflowName(p.View().RunID) || b.Workflow.Name == p.View().RunID {
			t.Error("native Create did not derive fixed name", b.Workflow.Name)
		}
		return b, err
	}
	q := startRequest("native-preparation", request())
	q.Request.Assist = true
	if _, err := f.new(t).StartRun(t.Context(), q); err != nil {
		t.Fatal(err)
	}
}

type nativeReadPort struct{ app.Runs }

func TestNativeCreateOwnsTheOnlyBinding(t *testing.T) {
	t.Parallel()
	if _, exists := reflect.TypeOf((*app.Runs)(nil)).Elem().MethodByName("BindWorkflow"); exists {
		t.Fatal("wiring consumes a binding writer")
	}
	f := newFixture(t)
	f.d.Runs = nativeReadPort{f.runs}
	writes := 0
	f.native.BindFunc = func(ctx context.Context, b nativebinding.RunBinding) error {
		writes++
		return f.runs.BindWorkflow(ctx, stateport.Action{Actor: request().Actor.Identity}, b)
	}
	if _, err := f.new(t).StartRun(t.Context(), startRequest("native-binds", request())); err != nil {
		t.Fatal(err)
	}
	b, ok := f.runs.Workflow(t.Context(), "r1")
	if writes != 1 || !ok || b.Name != nativebinding.WorkflowName("r1") {
		t.Fatal("native did not bind exactly once", writes, b, ok)
	}
}
func TestRecoveryKeepsSourcesBeforeNativeFindBinds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	p := mustAdmit(t, s, f)
	accepted(t, f, p)
	kept := false
	f.keeper.KeepFunc = func(context.Context, string, []sourcecontract.Binding) error { kept = true; return nil }
	f.native.BindFunc = func(ctx context.Context, b nativebinding.RunBinding) error {
		if !kept {
			t.Error("native recovery bound before retention")
		}
		return f.runs.BindWorkflow(ctx, stateport.Action{Actor: request().Actor.Identity}, b)
	}
	// New uses a value copy of the native port.
	s = f.new(t)
	if err := s.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.runs.Workflow(t.Context(), "r1"); !ok || f.creates.Load() != 0 {
		t.Fatal("recovery did not bind without Create")
	}
}
func TestNativeBindingOutageRemainsRecoverable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fail := true
	native := fake.NewNative(f.runs.Run, func(ctx context.Context, b nativebinding.RunBinding) error {
		if fail {
			return errors.New("binding storage unavailable")
		}
		return f.bindNative(ctx, b)
	})
	native.Hook = f.native.Hook
	create := native.CreateFunc
	native.CreateFunc = func(ctx context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		f.creates.Add(1)
		return create(ctx, p)
	}
	f.native = native
	s := f.new(t)
	out, err := s.StartRun(t.Context(), startRequest("native-binding-outage", request()))
	if err != nil || out.State != "accepted" {
		t.Fatal("binding outage became a failed run", out, err)
	}
	if _, failed := f.runs.Failure(t.Context(), "r1"); failed {
		t.Fatal("binding outage made the accepted run terminal")
	}
	fail = false
	s = f.new(t)
	if err := s.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.runs.Workflow(t.Context(), "r1"); !ok || f.creates.Load() != 1 {
		t.Fatal("recovery recreated the native execution")
	}
}

func TestUncertainNativeCreateIsRecoveredWithoutAnotherCreate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.native.CreateFunc = func(context.Context, *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		return nativebinding.RunBinding{}, publicerr.New(publicerr.Unavailable, "native reply unavailable", "Recover the accepted run.", nil)
	}
	s := f.new(t)
	out, err := s.StartRun(t.Context(), startRequest("uncertain-create", request()))
	if err != nil || out.State != "accepted" {
		t.Fatal(out, err)
	}
	if _, failed := f.runs.Failure(t.Context(), "r1"); failed {
		t.Fatal("uncertain native creation was recorded as a refusal")
	}
	s = f.new(t)
	if err := s.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, bound := f.runs.Workflow(t.Context(), "r1"); !bound {
		t.Fatal("uncertain native execution was not recovered")
	}
}

func TestCreatedWorkflowSurvivesPermanentBindingRefusal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	deny := true
	refusal := publicerr.New(publicerr.Forbidden, "binding was refused", "Restore binding access.", nil)
	if !publicerr.Permanent(refusal) {
		t.Fatal("fixture is not a permanent refusal")
	}
	native := fake.NewNative(f.runs.Run, func(ctx context.Context, b nativebinding.RunBinding) error {
		if deny {
			return refusal
		}
		return f.bindNative(ctx, b)
	})
	native.Hook = f.native.Hook
	create := native.CreateFunc
	native.CreateFunc = func(ctx context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		f.creates.Add(1)
		return create(ctx, p)
	}
	f.native = native
	s := f.new(t)
	out, err := s.StartRun(t.Context(), startRequest("created-before-binding-refusal", request()))
	if err != nil || out.State != "accepted" {
		t.Fatal("created Workflow was made terminal", out, err)
	}
	row, err := f.runs.Run(t.Context(), "r1")
	if err != nil || row.Status == "failed" || row.Origin.WorkflowUID != "" {
		t.Fatal(row, err)
	}
	deny = false
	s = f.new(t)
	if err := s.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, bound := f.runs.Workflow(t.Context(), "r1"); !bound || f.creates.Load() != 1 {
		t.Fatal("created Workflow was not recovered without another Create")
	}
}
