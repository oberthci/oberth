package cronsync_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argologging "github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/nativebinding"
)

func (f *fixture) child() *wfv1.Workflow {
	f.t.Helper()
	f.sync()
	parent := f.object(f.repo, "nightly")
	ctx := argologging.WithLogger(f.t.Context(), argologging.NewSlogLogger(argologging.Warn, argologging.Text))
	child := common.ConvertCronWorkflowToWorkflowWithProperties(ctx, parent, parent.Name+"-123", f.now)
	child.UID = "child-uid"
	child.Namespace = "ci"
	child.CreationTimestamp = metav1.NewTime(f.now)
	return child
}
func TestChildRequiresLiveOwnerAndFrozenFacts(t *testing.T) {
	f := newFixture(t)
	valid := f.child()
	for name, change := range map[string]func(*wfv1.Workflow){
		"child_uid":  func(w *wfv1.Workflow) { w.UID = "" },
		"owner_name": func(w *wfv1.Workflow) { w.Labels[nativebinding.NativeCronLabel] = "other" },
		"engine":     func(w *wfv1.Workflow) { w.Labels[nativebinding.EngineLabel] = "other" },
		"trigger":    func(w *wfv1.Workflow) { w.Labels[nativebinding.TriggerLabel] = "release" },
		"workflow_label": func(w *wfv1.Workflow) {
			w.Spec.WorkflowMetadata.LabelsFrom[nativebinding.RunLabel] = wfv1.LabelValueFrom{Expression: "other"}
		},
		"no_owner":     func(w *wfv1.Workflow) { w.OwnerReferences = nil },
		"replaced_uid": func(w *wfv1.Workflow) { w.OwnerReferences[0].UID = "replacement" },
		"wrong_kind":   func(w *wfv1.Workflow) { w.OwnerReferences[0].Kind = "Workflow" },
		"wrong_api":    func(w *wfv1.Workflow) { w.OwnerReferences[0].APIVersion = "example.invalid/v1" },
		"namespace":    func(w *wfv1.Workflow) { w.Namespace = "other" },
		"name":         func(w *wfv1.Workflow) { w.Name = "unrelated" },
		"time":         func(w *wfv1.Workflow) { w.CreationTimestamp = metav1.Time{} },
		"entry":        func(w *wfv1.Workflow) { w.Spec.Arguments.Parameters[0].Value = wfv1.AnyStringPtr("other") },
		"no_value":     func(w *wfv1.Workflow) { w.Spec.Arguments.Parameters[0].Value = nil },
		"value_from": func(w *wfv1.Workflow) {
			w.Spec.Arguments.Parameters[0].ValueFrom = &wfv1.ValueFrom{Expression: "entry"}
		},
		"default": func(w *wfv1.Workflow) { w.Spec.Arguments.Parameters[0].Default = wfv1.AnyStringPtr("nightly") },
		"sha":     func(w *wfv1.Workflow) { w.Labels[nativebinding.CommitLabel] = strings.Repeat("b", 40) },
		"run":     func(w *wfv1.Workflow) { w.Labels[nativebinding.RunLabel] = "other" },
		"tier":    func(w *wfv1.Workflow) { w.Labels[nativebinding.TierLabel] = "release" },
		"duplicate": func(w *wfv1.Workflow) {
			w.Spec.Arguments.Parameters = append(w.Spec.Arguments.Parameters, w.Spec.Arguments.Parameters[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid.DeepCopy()
			change(candidate)
			_, found, _, err := f.c.ChildBinding(t.Context(), candidate)
			var refusal *cronsync.ChildRefusal
			if found || !errors.As(err, &refusal) {
				t.Fatal("hostile child accepted", found, err)
			}
		})
	}
	proof, found, _, err := f.c.ChildBinding(t.Context(), valid)
	if err != nil || !found || proof.Run.RunID != valid.Name || proof.Parent.UID != string(valid.OwnerReferences[0].UID) || proof.Run.Revision.Commit != strings.Repeat("a", 40) || proof.ScheduleEntry != "nightly" {
		t.Fatal(proof, found, err)
	}
	proof.Run.SpecSHA256 = strings.Repeat("c", 64)
	if err := proof.Validate(); err != nil {
		t.Fatal("C5-completed proof is invalid", err)
	}
}
func TestChildBindsItsRevisionAndOrphanCannotGainInitialProof(t *testing.T) {
	f := newFixture(t)
	child := f.child()
	parent := f.object(f.repo, "nightly")
	parent.Spec.WorkflowMetadata.Labels[nativebinding.CommitLabel] = strings.Repeat("b", 40)
	if _, err := f.native.Update(t.Context(), parent, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	proof, found, _, err := f.c.ChildBinding(t.Context(), child)
	if err != nil || !found || proof.Run.Revision.Commit != strings.Repeat("a", 40) {
		t.Fatal("new parent revision changed child", proof, err)
	}
	if err := f.native.Delete(t.Context(), parent.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, found, _, err = f.c.ChildBinding(t.Context(), child)
	var refusal *cronsync.ChildRefusal
	if found || !errors.As(err, &refusal) {
		t.Fatal("orphan gained proof", found, err)
	}
	f.sync()
	_, found, _, err = f.c.ChildBinding(t.Context(), child)
	if found || !errors.As(err, &refusal) {
		t.Fatal("recreated parent gained the old child", found, err)
	}
}
func TestResolvedChildMetadataAcceptedAndOutageIsNotPermanent(t *testing.T) {
	f := newFixture(t)
	child := f.child()
	child.Labels[nativebinding.RunLabel] = child.Name
	if _, found, _, err := f.c.ChildBinding(t.Context(), child); err != nil || !found {
		t.Fatal(found, err)
	}
	outage := errors.New("API unavailable")
	f.hook = func(_ context.Context, verb, _ string) error {
		if verb == "get" {
			return outage
		}
		return nil
	}
	_, found, _, err := f.c.ChildBinding(t.Context(), child)
	var refusal *cronsync.ChildRefusal
	if found || !errors.Is(err, outage) || errors.As(err, &refusal) {
		t.Fatal("outage became proof refusal", found, err)
	}
}

func TestCronPauseAndChildBinding(t *testing.T) {
	f := newFixture(t)
	child := f.child()
	f.paused = true
	if err := f.c.SetPause(t.Context(), f.repo, "nightly"); err != nil {
		t.Fatal(err)
	}
	restarted, err := cronsync.New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	first, found, _, err := restarted.ChildBinding(t.Context(), child)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	again, found, _, err := restarted.ChildBinding(t.Context(), child)
	if err != nil || !found || first != again || first.Run.Workflow.UID != string(child.UID) {
		t.Fatal("restart changed child proof", found, err)
	}
	status, err := restarted.Status(t.Context(), f.repo, "nightly")
	if err != nil || !status.UserPaused {
		t.Fatal("restart lost native pause", status, err)
	}
}

func TestChildRequiresConfiguredNamespaceAndNonemptyLiveOwnerUID(t *testing.T) {
	for _, kind := range []string{"namespace", "uid", "parent_namespace"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			child := f.child()
			config := f.config
			config.Client = changedRead{Native: f.native, change: func(parent *wfv1.CronWorkflow) {
				if kind == "uid" {
					parent.UID = ""
				} else {
					parent.Namespace = "other"
				}
			}}
			switch kind {
			case "uid":
				child.OwnerReferences[0].UID = ""
			case "namespace":
				child.Namespace = "other"
			}
			c, err := cronsync.New(config)
			if err != nil {
				t.Fatal(err)
			}
			_, found, _, err := c.ChildBinding(t.Context(), child)
			var refusal *cronsync.ChildRefusal
			if found || !errors.As(err, &refusal) {
				t.Fatal("unverifiable live parent admitted", found, err)
			}
		})
	}
}
