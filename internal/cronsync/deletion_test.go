package cronsync_test

import (
	"context"
	"errors"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/nativebinding"
)

func TestRefusedExistingEntryIsSuspendedAndNeverPruned(t *testing.T) {
	for _, raw := range []string{"schedules: [{name: nightly, cron: '@daily'}]", "schedules: [{name: nightly, cron: '* * * * *', concurrencyPolicy: bad}]"} {
		f := newFixture(t)
		f.sync()
		before := f.object(f.repo, "nightly")
		f.put(f.repo, raw)
		f.sync()
		after := f.object(f.repo, "nightly")
		if !after.Spec.Suspend || after.UID != before.UID {
			t.Fatal("refused entry deleted or left active")
		}
		for _, action := range f.native.Actions() {
			if action.Verb == "delete" {
				t.Fatal("refusal authorized deletion")
			}
		}
	}
}
func TestAbsentForOtherPathOrRefNeverDeletes(t *testing.T) {
	for _, verb := range []string{"read", "resolve"} {
		t.Run(verb, func(t *testing.T) {
			f := newFixture(t)
			f.sync()
			before := f.object(f.repo, "nightly")
			f.gitHook = func(_ context.Context, operation, _ string) error {
				if operation == verb {
					return &gitport.AbsentError{Path: ".oberth/other.yaml", Ref: "refs/heads/other"}
				}
				return nil
			}
			f.sync()
			after := f.object(f.repo, "nightly")
			if !after.Spec.Suspend || after.UID != before.UID {
				t.Fatal("unrelated absence deleted or activated schedule")
			}
		})
	}
}
func TestColdFullSyncDeletesRemovedRepository(t *testing.T) {
	f := newFixture(t)
	f.sync()
	before := f.object(f.repo, "nightly")
	f.repos = nil
	restarted, err := cronsync.New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.SyncAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	actions := f.native.Actions()
	assertOrphanDeletion(t, actions[len(actions)-1], before)
}
func TestPruneDeletesOnlyRemovedOwnedEntry(t *testing.T) {
	f := newFixture(t)
	f.put(f.repo, "schedules: [{name: nightly, cron: '0 3 * * *'}, {name: old, cron: '0 4 * * *'}]")
	f.sync()
	old := f.object(f.repo, "old")
	kept := f.object(f.repo, "nightly")
	foreign := old.DeepCopy()
	foreign.Name = "foreign"
	foreign.UID = ""
	foreign.ResourceVersion = ""
	delete(foreign.Labels, nativebinding.ManagedLabel)
	if _, err := f.native.Create(t.Context(), foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.put(f.repo, "schedules: [{name: nightly, cron: '0 3 * * *'}]")
	f.sync()
	actions := f.native.Actions()
	assertOrphanDeletion(t, actions[len(actions)-1], old)
	if f.object(f.repo, "nightly").UID != kept.UID {
		t.Fatal("retained entry replaced")
	}
	if _, err := f.native.Get(t.Context(), foreign.Name, metav1.GetOptions{}); err != nil {
		t.Fatal("foreign schedule deleted", err)
	}
}

type observedNative struct {
	cronsync.Native
	list func(context.Context, metav1.ListOptions) (*wfv1.CronWorkflowList, error)
}

func (n observedNative) List(ctx context.Context, options metav1.ListOptions) (*wfv1.CronWorkflowList, error) {
	return n.list(ctx, options)
}
func TestIncompleteObservationNeverAuthorizesDeletion(t *testing.T) {
	for _, kind := range []string{"list_error", "catalog_error", "missing_uid"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.sync()
			before := f.object(f.repo, "nightly")
			f.repos = nil
			config := f.config
			config.Client = observedNative{Native: f.native, list: deletionObservation(before, kind)}
			if kind == "catalog_error" {
				f.catalogErr = errors.New("partial catalog")
			}
			attempted := false
			f.hook = func(_ context.Context, verb, _ string) error {
				if verb == "delete" {
					attempted = true
				}
				return nil
			}
			restarted, err := cronsync.New(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.SyncAll(t.Context()); err == nil || attempted {
				t.Fatal("incomplete observation authorized deletion", err)
			}
			if f.object(f.repo, "nightly").UID != before.UID {
				t.Fatal("schedule deleted")
			}
		})
	}
}

func deletionObservation(before *wfv1.CronWorkflow, kind string) func(context.Context, metav1.ListOptions) (*wfv1.CronWorkflowList, error) {
	return func(context.Context, metav1.ListOptions) (*wfv1.CronWorkflowList, error) {
		object := before.DeepCopy()
		if kind == "missing_uid" {
			object.UID = ""
		}
		result := &wfv1.CronWorkflowList{Items: []wfv1.CronWorkflow{*object}}
		if kind == "list_error" {
			return result, errors.New("partial list")
		}
		return result, nil
	}
}
