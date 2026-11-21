package contracttest

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
)

type NativeFixture struct {
	Port     app.Native
	Prepared *admissioncontract.PreparedAdmission
	Run      stateport.Run
	Stored   func(context.Context, string) (stateport.Run, error)
}

func Native(t *testing.T, setup func(*testing.T) NativeFixture) {
	t.Helper()
	t.Run("native", func(t *testing.T) {
		f := setup(t)
		ctx := t.Context()
		name := nativebinding.WorkflowName(f.Run.ID)
		first, err := f.Port.Create(ctx, f.Prepared)
		if err != nil || first.Workflow.Name != name || first.Workflow.UID == "" {
			t.Fatal(first, err)
		}
		stored, err := f.Stored(ctx, f.Run.ID)
		if err != nil || stored.Origin.WorkflowUID != first.Workflow.UID {
			t.Fatal("native Create did not bind", stored, err)
		}
		again, err := f.Port.Create(ctx, f.Prepared)
		if err != nil || first != again {
			t.Fatal("fixed-name create changed identity", again, err)
		}
		got, found, err := f.Port.Find(ctx, f.Run)
		if err != nil || !found || got != first {
			t.Fatal("owned object not found", got, err)
		}
		nativeStopAndOwner(t, f, first)
	})
}
func nativeStopAndOwner(t *testing.T, f NativeFixture, first nativebinding.RunBinding) {
	t.Helper()
	ctx := t.Context()
	foreign := f.Run.Clone()
	foreign.Admission.Repository = "other/team/widget"
	if _, _, err := f.Port.Find(ctx, foreign); err == nil {
		t.Fatal("foreign owner adopted")
	}
	wrong := first
	wrong.Workflow.UID = "foreign"
	if err := f.Port.Stop(ctx, wrong); err == nil {
		t.Fatal("foreign workflow stopped")
	}
	if err := f.Port.Terminate(ctx, wrong); err == nil {
		t.Fatal("foreign workflow terminated")
	}
	if err := f.Port.Stop(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := f.Port.Terminate(ctx, first); err != nil {
		t.Fatal(err)
	}
	got, found, err := f.Port.Find(ctx, f.Run)
	if err != nil || !found || got.Workflow.UID != first.Workflow.UID {
		t.Fatal("stop deleted or replaced object", err)
	}
}
