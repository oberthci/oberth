package cronsync_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/nativebinding"
)

func TestChangedSpecUpdatesSameUIDAndStatus(t *testing.T) {
	f := newFixture(t)
	f.sync()
	before := f.object(f.repo, "nightly")
	before.Status.LastScheduledTime = &metav1.Time{Time: f.now}
	if _, err := f.native.Update(t.Context(), before, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.put(f.repo, "schedules: [{name: nightly, cron: '5 4 * * *', timezone: Europe/Amsterdam, concurrencyPolicy: Forbid}]")
	f.sync()
	after := f.object(f.repo, "nightly")
	if after.UID != before.UID || !reflect.DeepEqual(after.Status, before.Status) || !reflect.DeepEqual(after.Spec.Schedules, []string{"5 4 * * *"}) || after.Spec.Timezone != "Europe/Amsterdam" || after.Spec.ConcurrencyPolicy != wfv1.ForbidConcurrent {
		t.Fatal("changed entry replaced identity or lost its new spec/status", after)
	}
}
func TestOuterWorkflowMetadataStripsRunAndReplacesControllerLabels(t *testing.T) {
	f := newFixture(t)
	config := f.config
	config.Admit = func(_ context.Context, request cronsync.Preparation) (*admissioncontract.PreparedAdmission, error) {
		view := prepared(request)
		view.Workflow.Labels[nativebinding.RunLabel] = "old-run"
		view.Workflow.Labels["workflows.argoproj.io/controller-instanceid"] = "other-controller"
		view.Workflow.Annotations[nativebinding.RunIDAnnotation] = "old-run"
		return admissioncontract.Seal(view)
	}
	var err error
	f.c, err = cronsync.New(config)
	if err != nil {
		t.Fatal(err)
	}
	f.sync()
	metadata := f.object(f.repo, "nightly").Spec.WorkflowMetadata
	if metadata.Labels["workflows.argoproj.io/controller-instanceid"] != config.InstanceID {
		t.Fatal("controller identity lost", metadata.Labels)
	}
	for _, key := range []string{nativebinding.RunLabel} {
		if _, present := metadata.Labels[key]; present {
			t.Fatal("propagated unsafe label", key)
		}
	}
	if _, present := metadata.Annotations[nativebinding.RunIDAnnotation]; present {
		t.Fatal("propagated old run identity")
	}
}
func TestChildRejectsLiveParentThatLostManagement(t *testing.T) {
	f := newFixture(t)
	child := f.child()
	parent := f.object(f.repo, "nightly")
	delete(parent.Labels, nativebinding.ManagedLabel)
	if _, err := f.native.Update(t.Context(), parent, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, found, _, err := f.c.ChildBinding(t.Context(), child)
	var refusal *cronsync.ChildRefusal
	if found || !errors.As(err, &refusal) {
		t.Fatal("unmanaged live schedule adopted", found, err)
	}
}
func TestFirstFailurePreservesUserPauseInStatus(t *testing.T) {
	f := newFixture(t)
	f.paused = true
	f.sync()
	f.admitErr = authorError()
	f.sync()
	status, err := f.c.Status(t.Context(), f.repo, "nightly")
	if err != nil || !status.UserPaused || !status.SyncSuspended || !status.Next.IsZero() {
		t.Fatal("first refusal lost user pause or displayed a next run", status, err)
	}
}
func TestMissingRunBoundaryAndSuspendedNext(t *testing.T) {
	f := newFixture(t)
	f.put(f.repo, "schedules: [{name: nightly, cron: '* * * * *'}]")
	f.sync()
	f.paused = true
	if err := f.c.SetPause(t.Context(), f.repo, "nightly"); err != nil {
		t.Fatal(err)
	}
	status, err := f.c.Status(t.Context(), f.repo, "nightly")
	if err != nil || !status.Next.IsZero() || status.Error != "" {
		t.Fatal("paused schedule advertises a next run", status, err)
	}
}
