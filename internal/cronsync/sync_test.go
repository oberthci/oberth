package cronsync_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/schedule"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func authorError() error {
	return publicerr.New(publicerr.InvalidInput, "pipeline is invalid", "Correct the pipeline and sync again.", nil)
}
func TestCronSyncGeneratedObjectGoldens(t *testing.T) {
	for _, policy := range []string{"", "Allow", "Forbid", "Replace"} {
		t.Run(policy, func(t *testing.T) {
			f := newFixture(t)
			f.put(f.repo, "schedules: [{name: nightly, cron: '0 3 * * *', concurrencyPolicy: '"+policy+"'}]")
			f.sync()
			object := f.object(f.repo, "nightly")
			want := prepared(f.calls[0]).Workflow.Spec
			if !reflect.DeepEqual(object.Spec.WorkflowSpec, want) || object.Spec.ConcurrencyPolicy != wfv1.ConcurrencyPolicy(policy) {
				t.Fatal("sealed spec or native policy changed")
			}
			if *object.Spec.StartingDeadlineSeconds != 86400 || *object.Spec.SuccessfulJobsHistoryLimit != 2147483647 || *object.Spec.FailedJobsHistoryLimit != 2147483647 {
				t.Fatal("native history/catch-up differs")
			}
			if len(object.Name) > 52 || object.Labels[nativebinding.ManagedLabel] != "oberth" || f.validationCalls != 1 {
				t.Fatal(object, f.validationCalls)
			}
			f.validationErr = authorError()
			f.sync()
			after := f.object(f.repo, "nightly")
			if !after.Spec.Suspend || !reflect.DeepEqual(after.Spec.WorkflowSpec, want) {
				t.Fatal("refusal replaced admitted spec")
			}
		})
	}
}
func TestUpdatePreservesUIDStatusAndNoop(t *testing.T) {
	f := newFixture(t)
	f.sync()
	first := f.object(f.repo, "nightly")
	first.Status.Conditions = wfv1.Conditions{{Type: "SubmissionError", Status: metav1.ConditionTrue, Message: "native detail"}}
	saved, err := f.native.Update(t.Context(), first, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	actions := len(f.native.Actions())
	f.sync()
	after := f.object(f.repo, "nightly")
	if after.UID != saved.UID || !reflect.DeepEqual(after.Status, saved.Status) || len(f.native.Actions()) != actions {
		t.Fatal("no-op changed native state")
	}
}
func TestCronFailuresRemainPending(t *testing.T) {
	outage := errors.New("store unavailable")
	for _, row := range []struct {
		name    string
		cause   error
		pending bool
	}{
		{"author", authorError(), false}, {"wrapped_author", errors.Join(authorError(), &gitport.AuthorRefusalError{}), false},
		{"mixed", errors.Join(authorError(), outage), true},
		{"public_mixed", errors.Join(authorError(), publicerr.New(publicerr.Unavailable, "store is unavailable", "Restore store access and retry.", nil)), true}, {"typed_outage", errors.Join(authorError(), &gitport.UnavailableError{Cause: outage}), true},
		{"cancel", errors.Join(authorError(), context.Canceled), true}, {"template", publicerr.New(publicerr.Unavailable, "template read failed", "Retry.", outage), true},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t)
			f.sync()
			before := f.object(f.repo, "nightly")
			f.admitErr = row.cause
			err := f.c.SyncAll(t.Context())
			if (err != nil) != row.pending {
				t.Fatal("wrong pending work", err)
			}
			after := f.object(f.repo, "nightly")
			if after.Spec.Suspend == row.pending || after.UID != before.UID || !reflect.DeepEqual(after.Spec.WorkflowSpec, before.Spec.WorkflowSpec) {
				t.Fatal("refusal changed admitted work")
			}
			status, statusErr := f.c.Status(t.Context(), f.repo, "nightly")
			want := "schedule cannot be prepared"
			if row.pending {
				want = "schedule admission is unavailable"
			}
			if statusErr != nil || status.Error != want || status.SyncSuspended == row.pending {
				t.Fatal("failure lost its producing stage or suspension state", status, statusErr)
			}

			f.admitErr = nil
			f.sync()
			if f.object(f.repo, "nightly").Spec.Suspend {
				t.Fatal("recovery did not converge")
			}
		})
	}
}
func TestReadFailureNeverDeletesAndMissingFileDeletesOnlyOwnedUID(t *testing.T) {
	f := newFixture(t)
	f.sync()
	before := f.object(f.repo, "nightly")
	outage := errors.New("read failed")
	f.gitHook = func(_ context.Context, verb, _ string) error {
		if verb == "read" {
			return outage
		}
		return nil
	}
	if err := f.c.SyncAll(t.Context()); !errors.Is(err, outage) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.object(f.repo, "nightly"), before) {
		t.Fatal("outage changed admitted parent")
	}
	f.gitHook = func(_ context.Context, verb, _ string) error {
		if verb == "read" {
			return &gitport.AbsentError{Repository: f.repo.Qualified, Path: schedule.FileName}
		}
		return nil
	}
	f.sync()
	actions := f.native.Actions()
	last := actions[len(actions)-1]
	assertOrphanDeletion(t, last, before)
}
func TestForeignParentNeverMutates(t *testing.T) {
	f := newFixture(t)
	f.sync()
	object := f.object(f.repo, "nightly")
	delete(object.Labels, nativebinding.ManagedLabel)
	if _, err := f.native.Update(t.Context(), object, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	actions := len(f.native.Actions())
	_ = f.c.SyncAll(t.Context())
	_ = f.c.SetPause(t.Context(), f.repo, "nightly")
	if len(f.native.Actions()) != actions {
		t.Fatal("foreign parent mutated")
	}
}
func TestPreparedBindingsAndSourceGuard(t *testing.T) {
	f := newFixture(t)
	file, err := schedule.Parse([]byte("schedules: [{name: nightly, cron: '* * * * *'}]"), schedule.Limits{}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	request := cronsync.Preparation{Repository: f.repo, Entry: file.Entries[0], Revision: gitport.Ref{Name: "refs/heads/main", ObjectSHA: strings.Repeat("a", 40), CommitSHA: strings.Repeat("a", 40)}}
	for _, change := range []func(*admissioncontract.PreparedView){
		func(v *admissioncontract.PreparedView) { v.RunID = "other" },
		func(v *admissioncontract.PreparedView) { v.Trigger = "push" },
		func(v *admissioncontract.PreparedView) { v.ObjectSHA = strings.Repeat("c", 40) },
		func(v *admissioncontract.PreparedView) { v.Ref = "refs/heads/other" },
		func(v *admissioncontract.PreparedView) { v.ScheduleEntry = "other" },
		func(v *admissioncontract.PreparedView) { v.CommitSHA = strings.Repeat("b", 40) },
		func(v *admissioncontract.PreparedView) { v.Repository = "origin/team/other" },
		func(v *admissioncontract.PreparedView) { v.SourceBindings = []sourcecontract.Binding{{Path: "main"}} },
	} {
		view := prepared(request)
		change(&view)
		sealed, err := admissioncontract.Seal(view)
		if err != nil {
			t.Fatal(err)
		}
		config := f.config
		config.Admit = func(context.Context, cronsync.Preparation) (*admissioncontract.PreparedAdmission, error) {
			return sealed, nil
		}
		f.c, err = cronsync.New(config)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.c.SyncAll(t.Context())
		statuses, err := f.c.Snapshot(t.Context(), f.repo)
		if err != nil || len(statuses) != 1 || statuses[0].Error == "" || len(f.native.Actions()) != 0 {
			t.Fatal("mismatched/source schedule accepted", statuses, err)
		}
	}
}
func TestScheduledSourceRefusalSuspendsSpecifically(t *testing.T) {
	f := newFixture(t)
	f.sync()
	f.admitErr = publicerr.New(publicerr.InvalidInput, "scheduled source is not supported", "Remove source steps or trigger manually.", nil)
	f.sync()
	object := f.object(f.repo, "nightly")
	if !object.Spec.Suspend {
		t.Fatal("specific source refusal lost")
	}
}
