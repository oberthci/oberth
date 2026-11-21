package cronsync_test

import (
	"context"
	"errors"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/nativebinding"
)

type changedRead struct {
	cronsync.Native
	change func(*wfv1.CronWorkflow)
}

func (n changedRead) Get(ctx context.Context, name string, options metav1.GetOptions) (*wfv1.CronWorkflow, error) {
	object, err := n.Native.Get(ctx, name, options)
	if err == nil {
		n.change(object)
	}
	return object, err
}
func TestPauseRefusesForeignNamespaceBeforeUpdate(t *testing.T) {
	f := newFixture(t)
	f.sync()
	f.paused = true
	attempted := false
	f.hook = func(_ context.Context, verb, _ string) error {
		if verb == "update" {
			attempted = true
		}
		return nil
	}
	config := f.config
	config.Client = changedRead{Native: f.native, change: func(object *wfv1.CronWorkflow) { object.Namespace = "elsewhere" }}
	c, err := cronsync.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetPause(t.Context(), f.repo, "nightly"); err == nil || attempted {
		t.Fatal("pause attempted a foreign namespace mutation", err)
	}
}
func TestPauseRequiresCurrentRepositoryAndReadableIntent(t *testing.T) {
	for _, kind := range []string{"missing", "id", "qualified", "catalog", "intent"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.sync()
			before := len(f.native.Actions())
			f.paused = true
			config := f.config
			config.Repository = func(context.Context, string) (cronsync.Repository, bool, error) {
				repo := f.repo
				switch kind {
				case "id":
					repo.ID++
				case "qualified":
					repo.Qualified = "origin/team/other"
				case "catalog":
					return repo, true, errors.New("catalog unavailable")
				}
				return repo, kind != "missing", nil
			}
			if kind == "intent" {
				f.pauseErr = errors.New("audit unavailable")
			}
			c, err := cronsync.New(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.SetPause(t.Context(), f.repo, "nightly"); err == nil || len(f.native.Actions()) != before {
				t.Fatal("pause applied without current identity/intent", err)
			}
		})
	}
}
func TestResumeRefusesPendingRepositoryOrFullRepair(t *testing.T) {
	for _, kind := range []string{"entry", "catalog"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.paused = true
			f.sync()
			f.paused = false
			if kind == "entry" {
				f.admitErr = errors.New("preparation unavailable")
			} else {
				f.catalogErr = errors.New("catalog unavailable")
			}
			if err := f.c.SyncAll(t.Context()); err == nil {
				t.Fatal("outage converged")
			}
			f.catalogErr = nil
			before := len(f.native.Actions())
			if err := f.c.SetPause(t.Context(), f.repo, "nightly"); err == nil || len(f.native.Actions()) != before {
				t.Fatal("resume cleared pending repair", err)
			}
		})
	}
}
func TestDeletionRequiresCompleteManagedIdentity(t *testing.T) {
	for _, kind := range []string{"label", "name", "parameter", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.sync()
			object := f.object(f.repo, "nightly")
			f.repos = nil
			switch kind {
			case "label":
				delete(object.Labels, nativebinding.ManagedLabel)
			case "name":
				object.Name = "unrelated"
			case "parameter":
				object.Spec.WorkflowSpec.Arguments.Parameters[0].Value = wfv1.AnyStringPtr("other")
			case "duplicate":
				object.Spec.WorkflowSpec.Arguments.Parameters = append(object.Spec.WorkflowSpec.Arguments.Parameters, object.Spec.WorkflowSpec.Arguments.Parameters[0])
			}
			config := f.config
			config.Client = observedNative{Native: f.native, list: deletionObservation(object, "")}
			attempted := false
			f.hook = func(_ context.Context, verb, _ string) error {
				if verb == "delete" {
					attempted = true
				}
				return nil
			}
			c, err := cronsync.New(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.SyncAll(t.Context()); err != nil || attempted {
				t.Fatal("deletion trusted partial identity", err)
			}
		})
	}
}
