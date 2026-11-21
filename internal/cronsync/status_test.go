package cronsync_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStatusIsReadOnlyAndNativeConditionsRemainVisible(t *testing.T) {
	f := newFixture(t)
	f.sync()
	object := f.object(f.repo, "nightly")
	object.CreationTimestamp = metav1.NewTime(f.now.Add(-25 * time.Hour))
	object.Status.Conditions = wfv1.Conditions{{Type: "SubmissionError", Status: metav1.ConditionTrue, Message: "native controller detail"}}
	if _, err := f.native.Update(t.Context(), object, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	actions := len(f.native.Actions())
	status, err := f.c.Status(t.Context(), f.repo, "nightly")
	if err != nil || status.Error != "schedule could not be updated" || !strings.Contains(status.Detail, "native controller detail") || status.Next.IsZero() {
		t.Fatal(status, err)
	}
	status.Conditions[0].Message = "changed"
	again, err := f.c.Status(t.Context(), f.repo, "nightly")
	if err != nil || again.Conditions[0].Message == "changed" || len(f.native.Actions()) != actions {
		t.Fatal("status changed native state", err)
	}
}
func TestSnapshotKeepsNativeFactsDuringUnreadableDeclaration(t *testing.T) {
	f := newFixture(t)
	f.sync()
	f.gitHook = func(context.Context, string, string) error { return errors.New("Git unavailable") }
	_ = f.c.SyncAll(t.Context())
	view, err := f.c.Snapshot(t.Context(), f.repo)
	if err != nil || len(view) != 2 || view[0].Error == "" || view[1].Entry != "nightly" || view[1].SyncSuspended {
		t.Fatal(view, err)
	}
}

func TestMissingParentHasItsOwnShortMessage(t *testing.T) {
	f := newFixture(t)
	status, err := f.c.Status(t.Context(), f.repo, "nightly")
	if err != nil || status.Error != "schedule is not active" || status.Detail == "" {
		t.Fatal(status, err)
	}
	if len(f.native.Actions()) != 0 {
		t.Fatal("status repaired missing parent")
	}
}
func TestPrivateFailureCauseIsNotRenderedInStatusOrAnnotation(t *testing.T) {
	f := newFixture(t)
	f.sync()
	private := errors.New("private dependency data")
	f.admitErr = private
	if err := f.c.SyncAll(t.Context()); !errors.Is(err, private) {
		t.Fatal("server lost private cause", err)
	}
	status, err := f.c.Status(t.Context(), f.repo, "nightly")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(status.Detail, private.Error()) {
		t.Fatal("private cause was rendered")
	}
}
