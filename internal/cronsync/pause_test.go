package cronsync_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestPauseReadFailurePreventsCreation(t *testing.T) {
	f := newFixture(t)
	f.pauseErr = errors.New("audit unavailable")
	if err := f.c.SyncAll(t.Context()); !errors.Is(err, f.pauseErr) || len(f.native.Actions()) != 0 {
		t.Fatal("created without audited intent", err)
	}
	f.pauseErr = nil
	f.paused = true
	f.sync()
	if !f.object(f.repo, "nightly").Spec.Suspend {
		t.Fatal("audited pause not applied")
	}
}
func TestPauseRecreationAndResumeDuringAuthorRefusal(t *testing.T) {
	f := newFixture(t)
	f.paused = true
	f.sync()
	first := f.object(f.repo, "nightly")
	if err := f.native.Delete(t.Context(), first.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.sync()
	recreated := f.object(f.repo, "nightly")
	if recreated.UID == first.UID || !recreated.Spec.Suspend {
		t.Fatal("recreation lost pause")
	}
	f.admitErr = authorError()
	f.sync()
	f.paused = false
	if err := f.c.SetPause(t.Context(), f.repo, "nightly"); err != nil {
		t.Fatal(err)
	}
	object := f.object(f.repo, "nightly")
	if !object.Spec.Suspend {
		t.Fatal("resume cleared refusal suspension")
	}
	f.admitErr = nil
	f.sync()
	if f.object(f.repo, "nightly").Spec.Suspend {
		t.Fatal("successful sync lost resume intent")
	}
}
func TestPauseWithoutParentAndColdRestart(t *testing.T) {
	f := newFixture(t)
	f.paused = true
	if err := f.c.SetPause(t.Context(), f.repo, "nightly"); err != nil {
		t.Fatal(err)
	}
	if len(f.native.Actions()) != 0 {
		t.Fatal("pause created parent")
	}
	f.sync()
	if !f.object(f.repo, "nightly").Spec.Suspend {
		t.Fatal("creation ignored persisted pause")
	}
}

func TestConflictRereadsLatestPauseIntent(t *testing.T) {
	f := newFixture(t)
	f.sync()
	f.admitErr = authorError()
	f.sync()
	f.admitErr = nil
	first := true
	f.hook = func(_ context.Context, verb, name string) error {
		if verb == "update" && first {
			first = false
			f.mu.Lock()
			f.paused = true
			f.mu.Unlock()
			return apierrors.NewConflict(schema.GroupResource{Resource: "cronworkflows"}, name, errors.New("changed"))
		}
		return nil
	}
	f.sync()
	if !f.object(f.repo, "nightly").Spec.Suspend {
		t.Fatal("conflict replay used stale pause intent")
	}
}

func TestMutationConflictBudgetAcceptsThirdAttemptAndRefusesFourth(t *testing.T) {
	for _, failures := range []int{2, 3} {
		t.Run(fmt.Sprint(failures), func(t *testing.T) {
			f := newFixture(t)
			f.sync()
			f.paused = true
			attempts := 0
			conflict := apierrors.NewConflict(schema.GroupResource{Resource: "cronworkflows"}, f.object(f.repo, "nightly").Name, errors.New("changed"))
			f.hook = func(_ context.Context, verb, _ string) error {
				if verb == "update" {
					attempts++
					if attempts <= failures {
						return conflict
					}
				}
				return nil
			}
			err := f.c.SetPause(t.Context(), f.repo, "nightly")
			if attempts != 3 || (err != nil) != (failures == 3) {
				t.Fatal(attempts, err)
			}
			if failures == 3 && !errors.Is(err, conflict) {
				t.Fatal("last native conflict cause lost", err)
			}
		})
	}
}

func TestRepeatedFailureDoesNotTurnServicePauseIntoUserPause(t *testing.T) {
	f := newFixture(t)
	f.sync()
	f.admitErr = authorError()
	f.sync()
	f.sync()
	status, err := f.c.Status(t.Context(), f.repo, "nightly")
	if err != nil || status.UserPaused || !status.SyncSuspended {
		t.Fatal("service pause became user intent", status, err)
	}
	f.admitErr = nil
	f.sync()
	status, err = f.c.Status(t.Context(), f.repo, "nightly")
	if err != nil || status.UserPaused || status.SyncSuspended || status.Next.IsZero() {
		t.Fatal("repair left a stale pause record", status, err)
	}
}
func TestPauseStopsOnNativeReadOrWriteFailure(t *testing.T) {
	for _, operation := range []string{"get", "update"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t)
			f.sync()
			f.paused = true
			before := len(f.native.Actions())
			attempts := 0
			outage := errors.New("native unavailable")
			f.hook = func(_ context.Context, verb, _ string) error {
				if verb == "update" {
					attempts++
				}
				if verb == operation {
					return outage
				}
				return nil
			}
			err := f.c.SetPause(t.Context(), f.repo, "nightly")
			want := 1
			if operation == "get" {
				want = 0
			}
			if !errors.Is(err, outage) || attempts != want || len(f.native.Actions()) != before {
				t.Fatal("pause ignored native failure or retried a non-conflict", attempts, err)
			}
		})
	}
}
